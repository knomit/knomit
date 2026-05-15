package store

import (
	"context"
	"path/filepath"
	"testing"

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
	c3, err := svc.Facts().DeleteFact(ctx, branch, "kb/e.md", "retract e")
	require.NoError(t, err)

	c4Res, err := svc.Facts().WriteFact(ctx, branch, "kb/f.md", testFactBody("f", 0.5, []string{"kb/e.md"}), "init f", "")
	require.NoError(t, err)
	c4 := c4Res.CommitHash
	require.NotEmpty(t, c3)

	// F's ref to E at c4: first ancestor touching E is c3 (deleted) → not ok.
	_, ok, err = si.resolveTargetCommit(ctx, branch, "kb/f.md", "kb/e.md", c4)
	require.NoError(t, err)
	require.False(t, ok, "first ancestor touching kb/e.md is a deletion → not ok")
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
