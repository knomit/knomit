package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestResolveTargetCommit covers the three outcomes from the design spec
// write-path step 2: ref to an existing path, ref to a never-created path,
// and ref to a deleted path.
func TestResolveTargetCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Seed kb/e.md added at c1.
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	// kb/d.md at c2 ref's kb/e.md.
	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "init d", "")
	require.NoError(t, err)
	c2 := c2Res.CommitHash

	si := svc.Search().(*searchIndex)

	// (1) D's ref to E at c2: most recent ancestor touching E is c1 (added) → target_commit = c1.
	got, ok, err := si.resolveTargetCommit(ctx, branch, "kb/d.md", "kb/e.md", c2)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, c1, got)

	// (2) Forward-broken: no ancestor touches kb/z.md.
	_, ok, err = si.resolveTargetCommit(ctx, branch, "kb/d.md", "kb/z.md", c2)
	require.NoError(t, err)
	require.False(t, ok, "no ancestor touches kb/z.md → not ok")

	// (3) Tombstoned: delete kb/e.md, then a later source ref's it.
	//
	// Per the historical-graph invariant: a retraction is just another write
	// event in the sparse history. The resolver must walk past the deletion
	// and find the prior valid version (c1, where e was added). Anchoring
	// edges to that prior version preserves the lineage — without this, refs
	// to retracted targets are silently dropped from the graph.
	c3, err := svc.Facts().DeleteFact(ctx, branch, "kb/e.md", "retract e")
	require.NoError(t, err)

	c4Res, err := svc.Facts().WriteFact(ctx, branch, "kb/f.md", testFactBody("f", 0.5, []string{"kb/e.md"}), "init f", "")
	require.NoError(t, err)
	c4 := c4Res.CommitHash
	require.NotEmpty(t, c3)

	got, ok, err = si.resolveTargetCommit(ctx, branch, "kb/f.md", "kb/e.md", c4)
	require.NoError(t, err)
	require.True(t, ok, "deletion is a write event — walk past it to the last valid version")
	require.Equal(t, c1, got, "must resolve to the commit where kb/e.md was added (c1), not stop at the retraction")
}

// TestResolveTargetCommit_WalksPastMultipleRetractions covers the case where
// a target was created, retracted, re-created, retracted again — the walk
// must skip all "deleted" rows and return the most recent "added/modified"
// ancestor. Mirrors the synthesize-review merge pattern where a fact's
// lineage refs span across retract cycles.
func TestResolveTargetCommit_WalksPastMultipleRetractions(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// c1: add kb/e.md (v1)
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e v1", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	// c2: retract kb/e.md
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/e.md", "retract e first time")
	require.NoError(t, err)

	// c3: re-add kb/e.md (v2)
	c3Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e v2", 0.9, nil), "re-add e", "")
	require.NoError(t, err)
	c3 := c3Res.CommitHash

	// c4: retract kb/e.md again
	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/e.md", "retract e second time")
	require.NoError(t, err)

	// c5: write kb/f.md with ref to kb/e.md (currently retracted at c4)
	c5Res, err := svc.Facts().WriteFact(ctx, branch, "kb/f.md", testFactBody("f", 0.5, []string{"kb/e.md"}), "init f", "")
	require.NoError(t, err)
	c5 := c5Res.CommitHash

	si := svc.Search().(*searchIndex)

	// Walk past the c4 retraction → land at c3 (the most recent add).
	got, ok, err := si.resolveTargetCommit(ctx, branch, "kb/f.md", "kb/e.md", c5)
	require.NoError(t, err)
	require.True(t, ok, "two retractions must not block resolution — walk past both")
	require.Equal(t, c3, got, "must resolve to the most recent ADD (c3), skipping the c4 retraction")

	_ = c1 // kept for clarity; c1 was the *first* add, not what we expect here
}

// TestResolveTargetCommit_SelfRef_ResolvesToPriorVersion regresses the bug
// where a fact's body listed its own path in refs and the resolver returned
// the source commit itself, producing a self-loop DERIVED_FROM edge whose
// source and target commits were identical. The semantically useful meaning
// of a self-ref is "this version derives from the previous version of this
// path" — so the walk must start at the source commit's first parent when
// refPath == sourcePath.
func TestResolveTargetCommit_SelfRef_ResolvesToPriorVersion(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// Initial creation of kb/x.md at c1 (no self-ref yet).
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/x.md", testFactBody("x v1", 0.9, nil), "init x", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	// Update at c2 with a self-ref.
	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/x.md", testFactBody("x v2", 0.9, []string{"kb/x.md"}), "update x with self-ref", "")
	require.NoError(t, err)
	c2 := c2Res.CommitHash

	si := svc.Search().(*searchIndex)

	got, ok, err := si.resolveTargetCommit(ctx, branch, "kb/x.md", "kb/x.md", c2)
	require.NoError(t, err)
	require.True(t, ok, "self-ref with a prior version must resolve")
	require.Equal(t, c1, got, "self-ref must resolve to the previous version's commit, not the source commit")
}

// TestResolveTargetCommit_SelfRef_FirstCreation_ReturnsNotOk covers the
// degenerate case where a fact's first version already lists its own path in
// refs. There is no prior version to derive from, so the resolver must drop
// the edge rather than producing a self-loop.
func TestResolveTargetCommit_SelfRef_FirstCreation_ReturnsNotOk(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/x.md", testFactBody("x v1", 0.9, []string{"kb/x.md"}), "init x with self-ref", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	si := svc.Search().(*searchIndex)

	_, ok, err := si.resolveTargetCommit(ctx, branch, "kb/x.md", "kb/x.md", c1)
	require.NoError(t, err)
	require.False(t, ok, "self-ref with no prior version must drop the edge")
}

// TestFactExistsAt covers the four cases the ref-kind resolver depends on:
//   - HEAD: live fact (branch_facts row present) → true
//   - HEAD: retracted at HEAD, but a prior version exists → true (walk-back)
//   - HEAD: never written → false
//   - commit-anchored: any prior add/modify in the ancestry → true
//   - commit-anchored: only retractions or no rows → false / true via walk-back
//
// This is the historical-graph existence predicate: a target retracted
// before the source's anchor must still classify as "exists" so refs
// to it render as `fact` (resolvable via fallback-before) instead of
// the misleading `broken`.
func TestFactExistsAt(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"
	search := svc.Search()

	// Never written.
	exists, err := search.FactExistsAt(ctx, branch, "kb/never.md", "")
	require.NoError(t, err)
	require.False(t, exists, "never-written path must not exist at HEAD")

	// c1: write kb/live.md.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/live.md", testFactBody("live", 0.9, nil), "init live", "")
	require.NoError(t, err)

	// c2: write kb/gone.md.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/gone.md", testFactBody("gone v1", 0.9, nil), "init gone", "")
	require.NoError(t, err)

	// c3: retract kb/gone.md (capture the retract commit for the anchored check).
	retractCommit, err := svc.Facts().DeleteFact(ctx, branch, "kb/gone.md", "retract gone")
	require.NoError(t, err)

	// HEAD: live → true.
	exists, err = search.FactExistsAt(ctx, branch, "kb/live.md", "")
	require.NoError(t, err)
	require.True(t, exists, "live path must exist at HEAD via branch_facts")

	// HEAD: retracted but historically reachable → true (walk-back).
	exists, err = search.FactExistsAt(ctx, branch, "kb/gone.md", "")
	require.NoError(t, err)
	require.True(t, exists, "retracted path with prior version must exist at HEAD via walk-back")

	// At the retract commit (commit-anchored): gone is retracted at this exact
	// commit, but a prior add exists in the ancestry — walk-back must surface it.
	exists, err = search.FactExistsAt(ctx, branch, "kb/gone.md", retractCommit)
	require.NoError(t, err)
	require.True(t, exists, "at the retract commit, gone resolves to its prior add via walk-back")

	// Never-written, commit-anchored: no add in any ancestor → false.
	exists, err = search.FactExistsAt(ctx, branch, "kb/never.md", retractCommit)
	require.NoError(t, err)
	require.False(t, exists, "never-written path must not exist at any commit anchor")
}

// testFactBody builds a minimal markdown fact for store-internal tests.
// Uses the existing fact.SerializeFact helper so the format matches what
// the parser expects, avoiding hand-rolled YAML drift.
func testFactBody(title string, conf float64, refs []string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Confidence = conf
	f.Sources = 1
	f.Domain = []string{"test"}
	f.Refs = refs
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// testFactBodyWithType is testFactBody plus an explicit epistemic type.
// Use when a test needs the indexed Fact node's type to be deterministic.
func testFactBodyWithType(title string, conf float64, refs []string, t fact.Type) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Confidence = conf
	f.Sources = 1
	f.Domain = []string{"test"}
	f.Refs = refs
	f.Type = t
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// TestGraphAddDerivedFromAtCommitTx_WritesEdgeWithBothCommits verifies that a
// single ref-event produces exactly one DERIVED_FROM edge with both
// source_commit and target_commit text properties set.
func TestGraphAddDerivedFromAtCommitTx_WritesEdgeWithBothCommits(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// E at c1, then D at c2 with refs=[E].
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	c2Res, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "init d", "")
	require.NoError(t, err)
	c2 := c2Res.CommitHash
	dBlobHash := c2Res.BlobHash

	si := svc.Search().(*searchIndex)

	// Reset DERIVED_FROM edges written by the upsert pipeline so we can
	// verify graphAddDerivedFromAtCommitTx's standalone behaviour without
	// the auto-wired edges interfering with the assertions below.
	_, err = si.rh.db.Exec(`DELETE FROM edge_props_text WHERE edge_id IN (SELECT id FROM edges WHERE type = ?)`, EdgeDerivedFrom)
	require.NoError(t, err)
	_, err = si.rh.db.Exec(`DELETE FROM edges WHERE type = ?`, EdgeDerivedFrom)
	require.NoError(t, err)

	// Manually invoke the new helper to write the edge with both commits.
	// (Task 4 will wire this into graphSyncFactTx; for this task's test we
	// drive it directly.)
	tx, err := si.rh.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, si.graphAddDerivedFromAtCommitTx(ctx, tx, branch, "kb/d.md", dBlobHash, c2, []string{"kb/e.md"}))
	require.NoError(t, tx.Commit())

	// Read back via Cypher: expect exactly one edge from D to E with both commit properties.
	rows, err := si.rh.db.QueryContext(ctx, `
		SELECT json_extract(value, '$.src'), json_extract(value, '$.sc'), json_extract(value, '$.tc')
		FROM json_each(cypher('MATCH (s:Fact)-[r:DERIVED_FROM]->(t:Fact {path: "kb/e.md"}) RETURN s.path AS src, r.source_commit AS sc, r.target_commit AS tc'))
	`)
	require.NoError(t, err)
	defer rows.Close()

	type edgeRow struct{ src, sc, tc string }
	var got []edgeRow
	for rows.Next() {
		var e edgeRow
		require.NoError(t, rows.Scan(&e.src, &e.sc, &e.tc))
		got = append(got, e)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 1)
	require.Equal(t, "kb/d.md", got[0].src)
	require.Equal(t, c2, got[0].sc)
	require.Equal(t, c1, got[0].tc)
}

// TestUpsert_WritesDerivedFromEdges_PostCommit regresses the bug where
// upsert's post-commit writePostCommitDerivedFrom silently failed because ctx
// still carried the committed *sql.Tx. conn(ctx, db) returned the closed tx,
// and QueryRowContext on it returned "transaction has already been committed or
// rolled back". The fix strips the tx from ctx via storegit.WithoutTx before
// any post-commit call, so conn falls through to the bare *sql.DB.
func TestUpsert_WritesDerivedFromEdges_PostCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// E first, then D ref'ing E.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/e.md", testFactBody("e", 0.9, nil), "init e", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/e.md"}), "d→e", "")
	require.NoError(t, err)

	si := svc.Search().(*searchIndex)
	var count int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&count))
	require.Equal(t, 1, count, "WriteFact should produce one DERIVED_FROM edge for D→E without needing Rebuild")
}

// TestGraphAddDerivedFromAtCommitTx_SkipsForwardBroken: ref to a path that
// has never been created produces no edge.
func TestGraphAddDerivedFromAtCommitTx_SkipsForwardBroken(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// D ref's a path that doesn't exist.
	dRes, err := svc.Facts().WriteFact(ctx, branch, "kb/d.md", testFactBody("d", 0.8, []string{"kb/never.md"}), "init d", "")
	require.NoError(t, err)
	c := dRes.CommitHash
	dBlobHash := dRes.BlobHash

	si := svc.Search().(*searchIndex)

	// Reset DERIVED_FROM edges written by the upsert pipeline so we can
	// verify graphAddDerivedFromAtCommitTx's standalone behaviour without
	// the auto-wired edges interfering with the assertions below.
	_, err = si.rh.db.Exec(`DELETE FROM edge_props_text WHERE edge_id IN (SELECT id FROM edges WHERE type = ?)`, EdgeDerivedFrom)
	require.NoError(t, err)
	_, err = si.rh.db.Exec(`DELETE FROM edges WHERE type = ?`, EdgeDerivedFrom)
	require.NoError(t, err)

	tx, err := si.rh.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	require.NoError(t, si.graphAddDerivedFromAtCommitTx(ctx, tx, branch, "kb/d.md", dBlobHash, c, []string{"kb/never.md"}))
	require.NoError(t, tx.Commit())

	var count int
	require.NoError(t, si.rh.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&count))
	require.Zero(t, count, "forward-broken ref must not produce any DERIVED_FROM edge")
}

// TestResolveActiveCommitForPath_DepthRegression asserts the resolver
// correctly finds the most-recent add/modify of `path` even when many
// unrelated commits sit between that write and the query anchor. The
// vtab-based implementation should handle this in a single SQL query;
// guards against accidental reintroduction of the old per-step SQL
// pattern that scaled O(walk-depth) in roundtrips.
func TestResolveActiveCommitForPath_DepthRegression(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	// Write the target once.
	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/target.md", testFactBody("t", 0.5, nil), "init", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	// Advance the branch tip 20 commits without touching target.
	var tip string
	for i := 0; i < 20; i++ {
		res, werr := svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/filler_%d.md", i),
			testFactBody(fmt.Sprintf("f%d", i), 0.5, nil),
			"filler", "")
		require.NoError(t, werr)
		tip = res.CommitHash
	}

	si := svc.Search().(*searchIndex)
	got, ok, err := si.resolveActiveCommitForPath(ctx, branch, "kb/target.md", tip)
	require.NoError(t, err)
	require.True(t, ok, "must resolve target through 20 unrelated commits")
	require.Equal(t, c1, got, "must resolve to the original add commit")
}

// TestResolveActiveCommitForPath_ConcurrentNoDeadlock guards against a class
// of bug where the resolver re-enters the *sql.DB connection pool from
// inside a query — e.g. via a Go-side virtual-table cursor whose Next()
// callback issues its own database/sql query. With db.SetMaxOpenConns(N),
// N concurrent walks would each hold one pool connection and block forever
// waiting for an (N+1)th. The whole pool wedges, downstream handlers like
// GET /branches hang on any subsequent WithRead. Run enough goroutines to
// safely exceed the configured pool size.
func TestResolveActiveCommitForPath_ConcurrentNoDeadlock(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	c1Res, err := svc.Facts().WriteFact(ctx, branch, "kb/target.md", testFactBody("t", 0.5, nil), "init", "")
	require.NoError(t, err)
	c1 := c1Res.CommitHash

	var tip string
	for i := 0; i < 30; i++ {
		res, werr := svc.Facts().WriteFact(ctx, branch,
			fmt.Sprintf("kb/filler_%d.md", i),
			testFactBody(fmt.Sprintf("f%d", i), 0.5, nil),
			"filler", "")
		require.NoError(t, werr)
		tip = res.CommitHash
	}

	si := svc.Search().(*searchIndex)

	const N = 8
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			got, ok, rerr := si.resolveActiveCommitForPath(ctx, branch, "kb/target.md", tip)
			if rerr != nil {
				errs <- fmt.Errorf("resolveActiveCommitForPath: %w", rerr)
				return
			}
			if !ok || got != c1 {
				errs <- fmt.Errorf("expected hash=%s ok=true, got hash=%s ok=%v", c1, got, ok)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("deadlock: %d concurrent resolveActiveCommitForPath calls did not complete within 15s", N)
	}
	close(errs)
	for e := range errs {
		t.Errorf("%v", e)
	}
}
