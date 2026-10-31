package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/fsck"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/merge"
)

// ref_edges (PR B) is the flat, per-BLOB mirror of the DERIVED_FROM edges, in
// the author's refs order. These tests pin: rows == the edges for every blob;
// the order is the refs order, not edge id order; rows are written once with
// the claim, replaced atomically with it; the real-blob invariant; backfill;
// rebuild == incremental; Verify's drift category; the batched level read.

// refRow is one ref_edges row as these tests compare it.
type refRow struct {
	Blob   string
	Ord    int
	Repo   string
	Path   string
	TBlob  string
	Commit string
}

func allRefRows(t *testing.T, db *sql.DB) []refRow {
	t.Helper()
	rows, err := db.Query(`SELECT source_blob, ord, target_repo, target_path, COALESCE(target_blob, ''), COALESCE(target_commit, '')
		FROM ref_edges ORDER BY source_blob, ord`)
	require.NoError(t, err)
	defer rows.Close()
	var out []refRow
	for rows.Next() {
		var r refRow
		require.NoError(t, rows.Scan(&r.Blob, &r.Ord, &r.Repo, &r.Path, &r.TBlob, &r.Commit))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func rowsOfBlob(rows []refRow, blob string) []refRow {
	var out []refRow
	for _, r := range rows {
		if r.Blob == blob {
			out = append(out, r)
		}
	}
	return out
}

// requireRowsMirrorEdges asserts the invariant for EVERY blob: the local
// ref_edges rows and the DERIVED_FROM edges are the same set of (source blob,
// target path, target blob, target commit), with no duplicate, and Verify's
// drift check agrees. It returns how many local rows it compared, so a
// caller can assert the comparison was not vacuous.
func requireRowsMirrorEdges(t *testing.T, svc *Service) int {
	t.Helper()
	db := svc.si.RepoForTest().DB()
	type k struct{ b, p, tb, tc string }
	edges := map[k]int{}
	for _, e := range allRefEdges(t, db) {
		edges[k{e.SrcBlob, e.TgtPath, e.TgtBlob, e.TargetCommit}]++
	}
	rows := map[k]int{}
	n := 0
	for _, r := range allRefRows(t, db) {
		if r.Repo != "" {
			require.Empty(t, r.TBlob, "a cross-KB row pins no version")
			require.Empty(t, r.Commit)
			continue
		}
		rows[k{r.Blob, r.Path, r.TBlob, r.Commit}]++
		n++
	}
	for key, c := range edges {
		require.Equal(t, 1, c, "one edge per (blob, target path, version): %+v", key)
	}
	require.Equal(t, edges, rows, "ref_edges == DERIVED_FROM edges, blob by blob")
	d, err := svc.si.RefEdgeDrift(context.Background())
	require.NoError(t, err)
	require.Zero(t, d.MissingRows+d.OrphanRows, "Verify's drift check agrees: %v", d.Examples)
	return n
}

// outOrder is OutgoingAtCommit's paths in order.
func outOrder(t *testing.T, svc *Service, branch, path, commit string) []string {
	t.Helper()
	out, err := svc.gq.OutgoingAtCommit(context.Background(), branch, path, commit)
	require.NoError(t, err)
	var ps []string
	for _, o := range out {
		ps = append(ps, o.Path)
	}
	return ps
}

// edgeIDOrder is the target paths of the blob's DERIVED_FROM edges in edge id
// order (what the edge read ordered by).
func edgeIDOrder(t *testing.T, db *sql.DB, blob string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT tp.value FROM edges e
		  JOIN node_props_text sb ON sb.node_id = e.source_id AND sb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
		  JOIN node_props_text tp ON tp.node_id = e.target_id AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
		 WHERE e.type = 'DERIVED_FROM' AND sb.value = ? ORDER BY e.id`, blob)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		out = append(out, p)
	}
	return out
}

// The one intended change of the switch: OutgoingAtCommit lists a version's
// references in the AUTHOR's order. F cites [kb/z.md, kb/b.md], z written in
// the same batch: Sync's pass 1 skips z (its node is not indexed yet) and pass
// 2 writes it later, so the edge ids run b, z (kb/gotchas/store/graph/
// derived-from-order). The edge read (OutgoingByEdgesForTest, same code but
// the read) returns b, z; the ref_edges read returns z, b. The SETS (with
// every field) are identical.
//
// Sabotage: ORDER BY r.target_path instead of r.ord in refEdgesOutOfBlob, and
// the order assertion fails.
func TestRefEdges_OutgoingFollowsTheAuthorsOrder(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/b.md", "B")
	refs := []string{"https://example.com/x", "kb/z.md", "kb/b.md"}
	commit, blobs, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{
		"kb/f.md": testFactBody("F", 0.8, refs),
		"kb/z.md": testFactBody("Z", 0.8, nil),
	}, nil, "f and z", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	fBlob := blobs["kb/f.md"]
	require.Equal(t, []string{"kb/b.md", "kb/z.md"}, edgeIDOrder(t, db, fBlob), "precondition: the edge ids are out of refs order")

	require.Equal(t, []string{"kb/z.md", "kb/b.md"}, outOrder(t, svc, "main", "kb/f.md", commit), "ref_edges: the author's order")
	byEdges, err := svc.gq.OutgoingByEdgesForTest(ctx, "main", "kb/f.md", commit)
	require.NoError(t, err)
	require.Equal(t, "kb/b.md", byEdges[0].Path, "the edge read: edge id order")
	byRows, err := svc.gq.OutgoingAtCommit(ctx, "main", "kb/f.md", commit)
	require.NoError(t, err)
	require.ElementsMatch(t, byEdges, byRows, "the same rows, every field")
	got := rowsOfBlob(allRefRows(t, db), fBlob)
	require.Len(t, got, 2)
	require.Equal(t, 1, got[0].Ord, "ord is the position in the refs list (the URL is ord 0)")
	require.Equal(t, 2, got[1].Ord)
	require.Equal(t, 2, requireRowsMirrorEdges(t, svc))
}

// A sweep over a store with history (edits, a retraction, renames, a merge,
// an experiment): for every (path, commit) in commit_log, OutgoingAtCommit
// equals the edge read as a multiset, every field, and its order is the
// refs order of the version.
func TestRefEdges_OutgoingEqualsTheEdgeReadEverywhere(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	write(t, svc, "agent/test", "kb/r.md", "R")
	write(t, svc, "agent/test", "kb/s.md", "S")
	write(t, svc, "agent/test", "kb/f.md", "F", "kb/s.md", "kb/r.md")
	write(t, svc, "agent/test", "kb/r.md", "R2")
	write(t, svc, "agent/test", "kb/g.md", "G", "kb/r.md", "kb/f.md", "kb/s.md")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	write(t, svc, "exp/x", "kb/h.md", "H", "kb/g.md", "kb/r.md")
	write(t, svc, "exp/x", "kb/f.md", "F2", "kb/r.md", "kb/g.md")
	_, err = svc.Merge().CommitExperiment(ctx, "x", nil)
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	_, err = svc.Facts().DeleteFact(ctx, "agent/test", "kb/s.md", "retract")
	require.NoError(t, err)
	_, _, err = svc.Facts().BatchWriteFacts(ctx, "agent/test", map[string]string{"kb/k.md": testFactBody("G", 0.8, []string{"kb/r.md", "kb/f.md", "kb/s.md"})},
		[]string{"kb/g.md"}, "rename g -> k", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))

	db := svc.si.RepoForTest().DB()
	rows, err := db.Query(`SELECT DISTINCT path, commit_hash FROM commit_log WHERE path LIKE 'kb/%'`)
	require.NoError(t, err)
	type pc struct{ p, c string }
	var all []pc
	for rows.Next() {
		var x pc
		require.NoError(t, rows.Scan(&x.p, &x.c))
		all = append(all, x)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	nonEmpty := 0
	for _, x := range all {
		a, err := svc.gq.OutgoingByEdgesForTest(ctx, "agent/test", x.p, x.c)
		require.NoError(t, err)
		b, err := svc.gq.OutgoingAtCommit(ctx, "agent/test", x.p, x.c)
		require.NoError(t, err)
		require.ElementsMatch(t, a, b, "%s@%.8s", x.p, x.c)
		if len(b) > 0 {
			nonEmpty++
		}
	}
	require.GreaterOrEqual(t, nonEmpty, 6, "the sweep compared real references")
	require.Greater(t, requireRowsMirrorEdges(t, svc), 6)
}

// Written ONCE per blob: a merge carrying a blob, a branch copy and its
// re-index, a rename, a retract-then-restore and an experiment commit add no
// row and change none.
//
// Sabotage: make writeRefRowsTx append a row whenever it runs (no sameRefRows
// short-circuit, INSERT OR IGNORE at a fresh ord), and the counts move.
func TestRefEdges_WrittenOncePerBlob(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "agent/test", "kb/r.md", "R")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "agent/test"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	_, fBlob := write(t, svc, "feature", "kb/f.md", "F", "kb/r.md", "kb://0123456789ab/kb/x.md")
	write(t, svc, "agent/test", "kb/r.md", "R2")
	before := allRefRows(t, db)
	require.Len(t, rowsOfBlob(before, fBlob), 2, "precondition: F's local and cross-KB rows")

	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "agent/test", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	require.Equal(t, before, allRefRows(t, db), "a merge carrying F adds nothing")

	require.NoError(t, svc.CreateBranch(ctx, "copy", "agent/test"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "copy"))
	require.Equal(t, before, allRefRows(t, db), "a branch copy adds nothing")

	_, blobs, err := svc.Facts().BatchWriteFacts(ctx, "agent/test",
		map[string]string{"kb/g.md": testFactBody("F", 0.8, []string{"kb/r.md", "kb://0123456789ab/kb/x.md"})},
		[]string{"kb/f.md"}, "rename f -> g", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	require.Equal(t, fBlob, blobs["kb/g.md"], "precondition: the same blob at a new path")
	require.Equal(t, before, allRefRows(t, db), "a rename adds nothing")

	_, err = svc.Facts().DeleteFact(ctx, "agent/test", "kb/g.md", "retract")
	require.NoError(t, err)
	write(t, svc, "agent/test", "kb/g.md", "F", "kb/r.md", "kb://0123456789ab/kb/x.md")
	require.Equal(t, before, allRefRows(t, db), "restore adds nothing")

	_, err = svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	_, eBlob := write(t, svc, "exp/x", "kb/e.md", "E", "kb/r.md")
	withE := allRefRows(t, db)
	require.Len(t, rowsOfBlob(withE, eBlob), 1)
	_, err = svc.Merge().CommitExperiment(ctx, "x", nil)
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	require.Equal(t, withE, allRefRows(t, db), "an experiment commit adds nothing")
	requireRowsMirrorEdges(t, svc)
}

// Cross-KB refs get a row (target_repo set, no pinned version), so a walk
// never parses frontmatter to find them; they have no edge, so outgoing (a
// local read) and the drift check ignore them. Non-fact refs get no row; a
// local ref cited twice keeps its first position.
func TestRefEdges_CrossKBRowsAreUnresolved(t *testing.T) {
	svc := newMergeTestStore(t)
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
	c, fBlob := write(t, svc, "main", "kb/f.md", "F",
		"kb://0123456789ab/kb/far.md", "src://0123456789ab/x.go@0000000000000000000000000000000000000000:0000000000000000000000000000000000000000",
		"kb/r.md", "https://example.com", "kb/r.md", "kb/missing.md")
	require.Equal(t, []refRow{
		{Blob: fBlob, Ord: 0, Repo: "0123456789ab", Path: "kb/far.md"},
		{Blob: fBlob, Ord: 2, Path: "kb/r.md", TBlob: rBlob, Commit: cR},
	}, rowsOfBlob(allRefRows(t, db), fBlob), "cross-KB row unresolved; the URL, the duplicate and the unresolved local ref get none")
	require.Equal(t, []string{"kb/r.md"}, outOrder(t, svc, "main", "kb/f.md", c), "outgoing lists local refs only, as before")

	// A version citing ONLY another KB has no edge but still gets its row,
	// written with a refs_commit claim at its introduction (user ruling, PR
	// #694: accepted).
	cG, gBlob := write(t, svc, "main", "kb/g.md", "G", "kb://0123456789ab/kb/far.md")
	require.Equal(t, []refRow{{Blob: gBlob, Ord: 0, Repo: "0123456789ab", Path: "kb/far.md"}}, rowsOfBlob(allRefRows(t, db), gBlob))
	require.Equal(t, cG, refsCommitOf(t, db, gBlob), "a cross-KB-only version is claimed like any other")
	require.Zero(t, countEdgesOfBlob(allRefEdges(t, db), gBlob), "and has no edge")
	requireRowsMirrorEdges(t, svc)
}

// A claim MOVE (an earlier introduction seen later) replaces the blob's rows
// with its edges, atomically: F's bytes are claimed first via g@c4 on
// "other", where r resolves to R2; the arrival at f@c1 (R) moves the claim,
// and the rows pin R@cR. With the rows' write failing inside the transaction,
// nothing of the move lands (claim, edges and rows stay at c4).
//
// Sabotage: write the rows with INSERT OR IGNORE and no DELETE, and the row
// still pins R2; commit the edges before the rows in a separate transaction,
// and the failed move leaves edges at c1 with rows at c4.
func TestRefEdges_ClaimMoveReplacesRowsAtomically(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
	c1, b := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	cR2, r2Blob := write(t, svc, "main", "kb/r.md", "R2")
	require.NoError(t, svc.CreateBranch(ctx, "other", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "other"))
	c4, b4 := write(t, svc, "other", "kb/g.md", "F", "kb/r.md")
	require.Equal(t, b, b4, "precondition: the same bytes")
	resetRefEdgesForTest(t, db)
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "other", "kb/g.md", b, c4, []string{"kb/r.md"}))
	require.Equal(t, c4, refsCommitOf(t, db, b), "precondition: claimed via g at c4")
	atC4 := []refRow{{Blob: b, Ord: 0, Path: "kb/r.md", TBlob: r2Blob, Commit: cR2}}
	require.Equal(t, atC4, rowsOfBlob(allRefRows(t, db), b), "precondition: the rows pin R2")

	reached := 0
	svc.si.SetBeforeRefRowsWriteHookForTest(func() error { reached++; return errors.New("crash before the rows") })
	err := svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", b, c1, []string{"kb/r.md"})
	require.Error(t, err)
	require.Equal(t, 1, reached, "the hook was reached: the move got as far as the rows")
	require.Equal(t, c4, refsCommitOf(t, db, b), "a failed move keeps the claim")
	require.Equal(t, map[string]string{"kb/r.md": r2Blob}, edgesOfBlob(allRefEdges(t, db), b), "and the edges")
	require.Equal(t, atC4, rowsOfBlob(allRefRows(t, db), b), "and the rows")

	svc.si.SetBeforeRefRowsWriteHookForTest(nil)
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", b, c1, []string{"kb/r.md"}))
	require.Equal(t, c1, refsCommitOf(t, db, b), "the move")
	require.Equal(t, []refRow{{Blob: b, Ord: 0, Path: "kb/r.md", TBlob: rBlob, Commit: cR}}, rowsOfBlob(allRefRows(t, db), b), "the rows follow the edges to R")
	requireRowsMirrorEdges(t, svc)
}

// A VOID claim (rolled-back experiment) re-claimed on the agent branch, after
// r changed there, replaces the rows: they pin R2 as resolved at the agent
// commit, not R as resolved in the gone experiment.
//
// Sabotage: skip writeRefRowsTx when the claim is replaced, and the row still
// pins R.
func TestRefEdges_VoidClaimReplacesRows(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	cE, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	require.Equal(t, []refRow{{Blob: fBlob, Ord: 0, Path: "kb/r.md", TBlob: rBlob, Commit: cR}}, rowsOfBlob(allRefRows(t, db), fBlob))
	require.NoError(t, svc.Merge().RollbackExperiment(ctx, "x"))
	require.Equal(t, cE, refsCommitOf(t, db, fBlob), "precondition: the void claim survives the rollback")

	cR2, r2Blob := write(t, svc, "agent/test", "kb/r.md", "R2")
	cA, b := write(t, svc, "agent/test", "kb/f.md", "F", "kb/r.md")
	require.Equal(t, fBlob, b)
	require.Equal(t, cA, refsCommitOf(t, db, fBlob), "re-claimed at the agent commit")
	require.Equal(t, []refRow{{Blob: fBlob, Ord: 0, Path: "kb/r.md", TBlob: r2Blob, Commit: cR2}}, rowsOfBlob(allRefRows(t, db), fBlob))
	requireRowsMirrorEdges(t, svc)
}

// Real-blob invariant: no row from a source blob missing from `objects`, and
// no row for a ref whose target blob is missing; the other refs keep theirs.
//
// Sabotage: build the local rows from the resolved targets including the
// skipped one (or write rows before the source-blob check), and a row
// appears.
func TestRefEdges_NoRowsForMissingBlobs(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, rBlob := write(t, svc, "main", "kb/r.md", "R")
	cS, sBlob := write(t, svc, "main", "kb/s.md", "S")
	_, err := db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, rBlob)
	require.NoError(t, err)
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md", "kb/s.md")
	require.Equal(t, []refRow{{Blob: fBlob, Ord: 1, Path: "kb/s.md", TBlob: sBlob, Commit: cS}}, rowsOfBlob(allRefRows(t, db), fBlob),
		"no row to the missing target; the other ref keeps its row")

	c, gBlob := write(t, svc, "main", "kb/g.md", "G", "kb/s.md", "kb://0123456789ab/kb/far.md")
	resetRefEdgesForTest(t, db)
	_, err = db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, gBlob)
	require.NoError(t, err)
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/g.md", gBlob, c, []string{"kb/s.md", "kb://0123456789ab/kb/far.md"}))
	require.Empty(t, rowsOfBlob(allRefRows(t, db), gBlob), "no rows at all from a source blob missing from objects, cross-KB included")
}

// Rebuild == incremental for ref_edges: rows captured from the incremental
// index equal the rows of a rebuild of every branch from wiped reference
// state, and equal the rows the backfill derives from the edges.
func TestRefEdges_RebuildAndBackfillEqualIncremental(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "agent/test", "kb/r.md", "R")
	write(t, svc, "agent/test", "kb/s.md", "S")
	commit, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", map[string]string{
		"kb/f.md": testFactBody("F", 0.8, []string{"kb/z.md", "kb://0123456789ab/kb/far.md", "kb/s.md", "kb/r.md"}),
		"kb/z.md": testFactBody("Z", 0.8, nil),
	}, nil, "f and z", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	write(t, svc, "agent/test", "kb/r.md", "R2")
	write(t, svc, "agent/test", "kb/f.md", "F2", "kb/r.md", "kb/z.md")
	write(t, svc, "agent/test", "kb/c.md", "C", "kb://0123456789ab/kb/only.md")
	_, err = svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	write(t, svc, "exp/x", "kb/h.md", "H", "kb/f.md", "kb/r.md")
	_ = commit

	inc := allRefRows(t, db)
	require.Greater(t, len(inc), 6, "precondition: rows to compare")
	requireRowsMirrorEdges(t, svc)

	// Backfill from the edges (a store indexed before ref_edges existed).
	_, err = db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	n, err := svc.si.BackfillRefEdges(ctx, "agent/test")
	require.NoError(t, err)
	require.Greater(t, n, 0)
	require.Equal(t, inc, allRefRows(t, db), "backfill == incremental (order and cross-KB rows included)")
	n, err = svc.si.BackfillRefEdges(ctx, "agent/test")
	require.NoError(t, err)
	require.Zero(t, n, "the sentinel: a second backfill does nothing")

	// Rebuild every branch from wiped reference state.
	resetRefEdgesForTest(t, db)
	for _, b := range []string{"agent/test", "main", "exp/x"} {
		require.NoError(t, svc.IndexManager().Rebuild(ctx, b, nil))
	}
	require.Equal(t, inc, allRefRows(t, db), "rebuild == incremental")
	requireRowsMirrorEdges(t, svc)
}

// An interrupted backfill (some blobs written, no sentinel) is completed by
// the next run, and the result equals the incremental rows.
func TestRefEdges_InterruptedBackfillResumes(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	for i := 0; i < 5; i++ {
		write(t, svc, "main", fmt.Sprintf("kb/f%d.md", i), fmt.Sprintf("F%d", i), "kb/r.md", "kb://0123456789ab/kb/far.md")
	}
	inc := allRefRows(t, db)
	require.Len(t, inc, 10)
	// The state a crash leaves: two blobs' rows written, the rest not, no
	// sentinel.
	keep := map[string]bool{inc[0].Blob: true, inc[4].Blob: true}
	for _, r := range inc {
		if !keep[r.Blob] {
			_, err := db.Exec(`DELETE FROM ref_edges WHERE source_blob = ?`, r.Blob)
			require.NoError(t, err)
		}
	}
	_, err := db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	require.Len(t, allRefRows(t, db), 4, "precondition: partly filled")
	n, err := svc.si.BackfillRefEdges(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, 3, n, "only the three missing blobs are written")
	require.Equal(t, inc, allRefRows(t, db))
}

// Verify reports drift between ref_edges and the edges as its own category
// (a deleted row, a row pinning another version, a row with no edge), and a
// rebuild repairs it.
//
// Sabotage: drop checkRefEdges from Verify (nothing reported), or drop
// repairRefEdges from rebuildGraph (the drift survives the rebuild).
func TestRefEdges_VerifyReportsDriftAndRebuildRepairsIt(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/s.md", "S")
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md", "kb/s.md")
	_, gBlob := write(t, svc, "main", "kb/g.md", "G", "kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil)) // spends the one-time v8 reset
	want := allRefRows(t, db)
	issues := func() []fsck.IntegrityIssue {
		rep, err := svc.Integrity().Verify(ctx, fsck.VerifyOpts{})
		require.NoError(t, err)
		var out []fsck.IntegrityIssue
		for _, i := range rep.Issues {
			if i.Category == fsck.CategoryRefEdges {
				out = append(out, i)
			}
		}
		return out
	}
	require.Empty(t, issues())

	// Plant: F loses its s row; G's row pins another commit; an orphan row.
	_, err := db.Exec(`DELETE FROM ref_edges WHERE source_blob = ? AND target_path = 'kb/s.md'`, fBlob)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE ref_edges SET target_commit = 'ffffffffffffffffffffffffffffffffffffffff' WHERE source_blob = ?`, gBlob)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ref_edges(source_blob, ord, target_repo, target_path, target_blob, target_commit) VALUES (?, 9, '', 'kb/r.md', ?, ?)`,
		"0123456789abcdef0123456789abcdef01234567", rBlob, cR)
	require.NoError(t, err)
	d, err := svc.si.RefEdgeDrift(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, d.MissingRows, "F's s edge and G's r edge have no identical row")
	require.Equal(t, 2, d.OrphanRows, "G's wrong row and the planted row have no edge")
	require.Len(t, d.Blobs, 3)
	got := issues()
	require.Len(t, got, 1, "one ref-edges issue")
	require.Contains(t, got[0].Detail, "2 edge(s) without a row, 2 row(s) without an edge, over 3 blob(s)")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Empty(t, issues(), "the rebuild repaired the drift")
	require.Equal(t, want, allRefRows(t, db))
}

// RefEdgesOfBlobs, the FactSet's level read: every row of a set of blobs in
// (source_blob, ord) order, the same across chunk boundaries (chunk = 2 over
// 5 blobs runs 3 statements), duplicates and unknown blobs ignored.
//
// Sabotage: process only the first chunk, or skip the sort of the keys, and
// the chunked answer differs.
func TestRefEdges_LevelReadIsChunkIndependent(t *testing.T) {
	svc := newMergeTestStore(t)
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/s.md", "S")
	var blobs []string
	for i := 0; i < 5; i++ {
		refs := []string{"kb/s.md", "kb/r.md"}
		if i%2 == 1 {
			refs = []string{"kb/r.md", "kb://0123456789ab/kb/far.md", "kb/s.md"}
		}
		_, b := write(t, svc, "main", fmt.Sprintf("kb/f%d.md", i), fmt.Sprintf("F%d", i), refs...)
		blobs = append(blobs, b)
	}
	ask := append([]string{"0000000000000000000000000000000000000000", blobs[3], ""}, blobs...)
	whole, err := svc.gq.RefEdgesOfBlobs(context.Background(), ask)
	require.NoError(t, err)
	require.Len(t, whole, 2*3+3*2)
	require.True(t, sort.SliceIsSorted(whole, func(i, j int) bool {
		if whole[i].SourceBlob != whole[j].SourceBlob {
			return whole[i].SourceBlob < whole[j].SourceBlob
		}
		return whole[i].Ord < whole[j].Ord
	}), "(source_blob, ord) order")
	var flat []refRow
	for _, r := range whole {
		flat = append(flat, refRow{r.SourceBlob, r.Ord, r.Repo, r.Path, r.TargetBlob, r.TargetCommit})
	}
	require.Equal(t, allRefRows(t, db), flat, "every row of the five blobs, exactly the table's")

	old := index.SetRefEdgesChunkForTest(2)
	defer index.SetRefEdgesChunkForTest(old)
	chunked, err := svc.gq.RefEdgesOfBlobs(context.Background(), ask)
	require.NoError(t, err)
	require.Equal(t, whole, chunked, "chunk-independent")
}

// A revisit of a claimed blob at its claim (Sync's pass 2, a rebuild's phase
// B, a re-index) whose edges are all present but whose rows are not current
// writes the rows; with rows current it writes nothing (no transaction).
//
// Sabotage: let applyBlobRefs's no-op check ignore the rows (return as soon
// as every edge exists), and the deleted rows stay deleted.
func TestRefEdges_RevisitAtTheClaimFillsMissingRows(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	c, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md", "kb://0123456789ab/kb/far.md")
	want := rowsOfBlob(allRefRows(t, db), fBlob)
	require.Len(t, want, 2)
	_, err := db.Exec(`DELETE FROM ref_edges WHERE source_blob = ?`, fBlob)
	require.NoError(t, err)

	reached := 0
	svc.si.SetBeforeRefRowsWriteHookForTest(func() error { reached++; return nil })
	defer svc.si.SetBeforeRefRowsWriteHookForTest(nil)
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", fBlob, c, []string{"kb/r.md", "kb://0123456789ab/kb/far.md"}))
	require.Equal(t, 1, reached, "the revisit opened the write transaction")
	require.Equal(t, want, rowsOfBlob(allRefRows(t, db), fBlob), "the rows are back")

	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", fBlob, c, []string{"kb/r.md", "kb://0123456789ab/kb/far.md"}))
	require.Equal(t, 1, reached, "with rows current, a revisit writes nothing")
}

// An edge whose target path is not among the source blob's refs (corruption
// only: the write path never makes one) is drift Verify reports, and a rebuild
// DROPS it (user ruling, PR #694: an edge that should not be there should not
// be there), leaving no drift and the blob's real edge and rows untouched.
//
// Sabotage: call repairRefBlobs with dropStray=false from repairRefEdges, and
// the stray edge survives the rebuild (and Verify keeps reporting it).
func TestRefEdges_RebuildDropsAnEdgeNotInTheRefs(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
	cS, sBlob := write(t, svc, "main", "kb/s.md", "S")
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil)) // spends the one-time v8 reset
	wantRows := allRefRows(t, db)
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(allRefEdges(t, db), fBlob))

	// Plant F -> S, a path F does not cite.
	src, err := svc.si.GraphNodeIDByBlobForTest(ctx, "kb/f.md", fBlob)
	require.NoError(t, err)
	tgt, err := svc.si.GraphNodeIDByBlobForTest(ctx, "kb/s.md", sBlob)
	require.NoError(t, err)
	id, err := svc.si.GraphInsertEdgeReturningIDForTest(ctx, src, tgt, index.EdgeDerivedFrom)
	require.NoError(t, err)
	require.NoError(t, svc.si.GraphSetEdgePropsForTest(ctx, id, map[string]string{"source_commit": cR, "target_commit": cS}))
	require.Equal(t, map[string]string{"kb/r.md": rBlob, "kb/s.md": sBlob}, edgesOfBlob(allRefEdges(t, db), fBlob), "precondition: the stray edge is there")

	rep, err := svc.Integrity().Verify(ctx, fsck.VerifyOpts{})
	require.NoError(t, err)
	reported := 0
	for _, i := range rep.Issues {
		if i.Category == fsck.CategoryRefEdges {
			reported++
			require.Contains(t, i.Detail, "1 edge(s) without a row, 0 row(s) without an edge")
		}
	}
	require.Equal(t, 1, reported, "Verify reports the stray edge as drift")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(allRefEdges(t, db), fBlob), "the rebuild dropped the stray edge, kept the real one")
	var props int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM edge_props_text WHERE edge_id = ?`, id).Scan(&props))
	require.Zero(t, props, "its properties went with it")
	require.Equal(t, wantRows, allRefRows(t, db), "the rows are unchanged")
	requireRowsMirrorEdges(t, svc)
}

// plantEdge adds a DERIVED_FROM edge (with its commit props) from the node of
// (srcPath, srcBlob) to the node of (tgtPath, tgtBlob), as corruption would.
func plantEdge(t *testing.T, svc *Service, srcPath, srcBlob, tgtPath, tgtBlob, sourceCommit, targetCommit string) int64 {
	t.Helper()
	ctx := context.Background()
	src, err := svc.si.GraphNodeIDByBlobForTest(ctx, srcPath, srcBlob)
	require.NoError(t, err)
	require.NotZero(t, src)
	tgt, err := svc.si.GraphNodeIDByBlobForTest(ctx, tgtPath, tgtBlob)
	require.NoError(t, err)
	require.NotZero(t, tgt)
	id, err := svc.si.GraphInsertEdgeReturningIDForTest(ctx, src, tgt, index.EdgeDerivedFrom)
	require.NoError(t, err)
	require.NoError(t, svc.si.GraphSetEdgePropsForTest(ctx, id, map[string]string{"source_commit": sourceCommit, "target_commit": targetCommit}))
	return id
}

// edgesOfBlobAll is every edge of the blob as "path@target_commit".
func edgesOfBlobAll(t *testing.T, db *sql.DB, blob string) []string {
	t.Helper()
	var out []string
	for _, e := range allRefEdges(t, db) {
		if e.SrcBlob == blob {
			out = append(out, e.TgtPath+"@"+e.TargetCommit)
		}
	}
	sort.Strings(out)
	return out
}

func refDrift(t *testing.T, svc *Service) index.RefEdgeDrift {
	t.Helper()
	d, err := svc.si.RefEdgeDrift(context.Background())
	require.NoError(t, err)
	return d
}

// PR #694 review F1: with the repo id unresolved, kb://<self>/ refs read as
// cross-KB. A rebuild then must drop NO edge (the repair is skipped) and
// rewrite no row (the write path leaves rows alone); once the id resolves
// again the next rebuild repairs normally. The unresolved state is a failing
// root-commit lookup (the real trigger), through a seam.
//
// Sabotage: drop the empty-repo-id guard in repairRefBlobs (F's valid
// kb://self edge is deleted), or set rowsOK := true in
// graphAddDerivedFromAtCommitTx (F's local row becomes a cross-KB row).
func TestRefEdges_UnresolvedRepoIDDropsAndRewritesNothing(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
	cS, sBlob := write(t, svc, "main", "kb/s.md", "S")
	root, err := svc.si.RepoForTest().RootCommit(ctx, "main")
	require.NoError(t, err)
	self := fact.ID12(root)
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb://"+self+"/kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil)) // spends the one-time v8 reset
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(allRefEdges(t, db), fBlob), "precondition: the kb://self edge exists")
	wantRows := []refRow{{Blob: fBlob, Ord: 0, Path: "kb/r.md", TBlob: rBlob, Commit: cR}}
	require.Equal(t, wantRows, rowsOfBlob(allRefRows(t, db), fBlob))
	stray := plantEdge(t, svc, "kb/f.md", fBlob, "kb/s.md", sBlob, cR, cS)
	require.Equal(t, 1, refDrift(t, svc).MissingRows, "precondition: drift for the repair to act on")

	lookups := 0
	svc.si.SetRootCommitForTest(func(context.Context, string) (string, error) {
		lookups++
		return "", errors.New("root commit unavailable")
	})
	require.Empty(t, svc.si.LocalRepoIDForTest(ctx, "main"), "precondition: the repo id is unresolved")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Greater(t, lookups, 1, "the failing lookup was retried, not cached")
	require.Equal(t, []string{"kb/r.md@" + cR, "kb/s.md@" + cS}, edgesOfBlobAll(t, db, fBlob), "no edge dropped: the valid kb://self edge survives (and so does the stray one)")
	require.Equal(t, wantRows, rowsOfBlob(allRefRows(t, db), fBlob), "no row rewritten from the wrong specs")

	svc.si.SetRootCommitForTest(nil)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, []string{"kb/r.md@" + cR}, edgesOfBlobAll(t, db, fBlob), "with the id back, the repair drops the stray edge only")
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM edges WHERE id = ?`, stray).Scan(&n))
	require.Zero(t, n)
	require.Equal(t, wantRows, rowsOfBlob(allRefRows(t, db), fBlob))
	requireRowsMirrorEdges(t, svc)
}

// localRepoID retries a failed lookup on the next call and caches a success.
//
// Sabotage: cache the failure (return the cached "" without calling the
// lookup again), and the second call stays "".
func TestRefEdges_LocalRepoIDRetriesAFailedLookup(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	root, err := svc.si.RepoForTest().RootCommit(ctx, "main")
	require.NoError(t, err)
	calls := 0
	svc.si.SetRootCommitForTest(func(ctx context.Context, branch string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("transient")
		}
		return root, nil
	})
	defer svc.si.SetRootCommitForTest(nil)
	require.Empty(t, svc.si.LocalRepoIDForTest(ctx, "main"), "the first lookup fails")
	require.Equal(t, fact.ID12(root), svc.si.LocalRepoIDForTest(ctx, "main"), "the next call retries")
	require.Equal(t, fact.ID12(root), svc.si.LocalRepoIDForTest(ctx, "main"))
	require.Equal(t, 2, calls, "a success is cached")
}

// PR #694 review N1: a blob that does not parse drops NOTHING on rebuild,
// although it carries a stray edge: its refs are unknown. (A blob MISSING
// from objects loses its edges to dropDanglingRefEdges anyway, before the
// repair, so "drops nothing" is not observable for it; the parse case pins
// the guard.)
//
// Sabotage: `dropStray || parsed[b]`, and the unparsable version loses both
// edges.
func TestRefEdges_RebuildDropsNothingFromAnUnparsableBlob(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, _ := write(t, svc, "main", "kb/r.md", "R")
	cS, sBlob := write(t, svc, "main", "kb/s.md", "S")
	c1, v1 := write(t, svc, "main", "kb/f.md", "F v1", "kb/r.md")
	write(t, svc, "main", "kb/f.md", "F v2", "kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil)) // spends the one-time v8 reset
	require.Equal(t, []string{"kb/r.md@" + cR}, edgesOfBlobAll(t, db, v1))
	plantEdge(t, svc, "kb/f.md", v1, "kb/s.md", sBlob, c1, cS)
	_, err := db.Exec(`UPDATE objects SET data = CAST('this is not a fact' AS BLOB) WHERE hash = ? AND type = 3`, v1)
	require.NoError(t, err)
	require.Contains(t, refDrift(t, svc).Blobs, v1, "precondition: the repair is asked to rewrite v1")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, []string{"kb/r.md@" + cR, "kb/s.md@" + cS}, edgesOfBlobAll(t, db, v1), "an unparsable blob drops nothing")
}

// PR #694 review N1: the heal's backfill never drops an edge, even a stray
// one; the edge stays reported as drift until a rebuild.
//
// Sabotage: `dropStray || parsed[b]`, and the backfill drops the stray edge.
func TestRefEdges_BackfillNeverDropsEdges(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, _ := write(t, svc, "main", "kb/r.md", "R")
	cS, sBlob := write(t, svc, "main", "kb/s.md", "S")
	cF, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	plantEdge(t, svc, "kb/f.md", fBlob, "kb/s.md", sBlob, cF, cS)
	_, err := db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	n, err := svc.si.BackfillRefEdges(ctx, "main")
	require.NoError(t, err)
	require.Greater(t, n, 0, "the backfill ran")
	require.Equal(t, []string{"kb/r.md@" + cR, "kb/s.md@" + cS}, edgesOfBlobAll(t, db, fBlob), "the backfill dropped nothing")
	require.Equal(t, 1, refDrift(t, svc).MissingRows, "the stray edge is still reported")
}

// PR #694 review N2: on a store upgraded from before ref_edges (rows empty,
// sentinel unset: the heal's backfill has not run yet), outgoing reads and the
// level read answer exactly as after the backfill, author's order included. A
// new store has the sentinel from the migration, so it reads the table.
//
// Sabotage: drop the fallback in OutgoingAtCommit (old versions have no refs)
// or in RefEdgesOfBlobs (no rows).
func TestRefEdges_ReadsAreTheSameBeforeAndAfterTheBackfill(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	var v string
	require.NoError(t, db.QueryRow(`SELECT value FROM meta WHERE key = 'ref_edges_backfilled'`).Scan(&v))
	require.Equal(t, "1", v, "a new store is marked backfilled by the migration")
	write(t, svc, "main", "kb/b.md", "B")
	commit, blobs, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{
		"kb/f.md": testFactBody("F", 0.8, []string{"https://example.com/x", "kb/z.md", "kb://0123456789ab/kb/far.md", "kb/b.md"}),
		"kb/z.md": testFactBody("Z", 0.8, nil),
	}, nil, "f and z", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	cG, gBlob := write(t, svc, "main", "kb/g.md", "G", "kb/f.md", "kb/b.md")
	write(t, svc, "main", "kb/f.md", "F2", "kb/b.md")
	fBlob := blobs["kb/f.md"]
	require.Equal(t, []string{"kb/b.md", "kb/z.md"}, edgeIDOrder(t, db, fBlob), "precondition: edge ids out of refs order")
	type q struct{ p, c string }
	asks := []q{{"kb/f.md", commit}, {"kb/g.md", cG}}
	read := func() ([][]index.RefSummary, []index.RefEdgeRow) {
		var outs [][]index.RefSummary
		for _, a := range asks {
			o, err := svc.gq.OutgoingAtCommit(ctx, "main", a.p, a.c)
			require.NoError(t, err)
			outs = append(outs, o)
		}
		rows, err := svc.gq.RefEdgesOfBlobs(ctx, []string{fBlob, gBlob})
		require.NoError(t, err)
		return outs, rows
	}
	wantOut, wantRows := read()
	require.Equal(t, "kb/z.md", wantOut[0][0].Path, "the author's order")
	require.Len(t, wantRows, 5, "F: z, far, b; G: f, b")

	_, err = db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	gotOut, gotRows := read()
	require.Equal(t, wantOut, gotOut, "outgoing before the backfill == after")
	require.Equal(t, wantRows, gotRows, "the level read before the backfill == after")

	_, err = svc.si.BackfillRefEdges(ctx, "main")
	require.NoError(t, err)
	gotOut, gotRows = read()
	require.Equal(t, wantOut, gotOut)
	require.Equal(t, wantRows, gotRows)
}

// PR #694 review N3: a second edge from one blob to one path (not producible
// by the write path: the resolver ignores the branch and the claim fixes the
// source commit) is drift a rebuild now converges: the first edge (the one
// the row is built from) is kept, the second is dropped, drift 0.
//
// Sabotage: keep every edge whose path is a local ref (drop the kept[p]
// check), and the duplicate survives the rebuild.
func TestRefEdges_RebuildKeepsOneEdgePerPath(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
	cR2, r2Blob := write(t, svc, "main", "kb/r.md", "R2")
	// F pins R2 (written after it); plant a second F -> r edge to the old R.
	cF, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil)) // spends the one-time v8 reset
	require.Equal(t, []string{"kb/r.md@" + cR2}, edgesOfBlobAll(t, db, fBlob))
	plantEdge(t, svc, "kb/f.md", fBlob, "kb/r.md", rBlob, cF, cR)
	require.Equal(t, 1, refDrift(t, svc).MissingRows, "precondition: the duplicate is drift")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, []string{"kb/r.md@" + cR2}, edgesOfBlobAll(t, db, fBlob), "the first edge is kept, the duplicate dropped")
	require.Equal(t, []refRow{{Blob: fBlob, Ord: 0, Path: "kb/r.md", TBlob: r2Blob, Commit: cR2}}, rowsOfBlob(allRefRows(t, db), fBlob))
	requireRowsMirrorEdges(t, svc)
}

// PR #694 review N4: the v8 reset clears the backfill sentinel, so a version
// on no branch (a rolled-back experiment's) gets its cross-KB row back from
// the backfill instead of losing it for good. Since review round 2 the
// rebuild that spends the reset runs that backfill itself (an explicit full
// rebuild never reaches the heal's), so after it the sentinel is set again
// and the row is back.
//
// Sabotage: keep the sentinel in resetBlobRefsOnce (the backfill is skipped
// and the row stays lost), or drop the backfill at the end of rebuildGraph
// (the sentinel stays unset).
func TestRefEdges_ResetReopensTheBackfillForOrphanVersions(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	_, eBlob := write(t, svc, "exp/x", "kb/e.md", "E", "kb://0123456789ab/kb/far.md")
	want := []refRow{{Blob: eBlob, Ord: 0, Repo: "0123456789ab", Path: "kb/far.md"}}
	require.Equal(t, want, rowsOfBlob(allRefRows(t, db), eBlob))
	require.NoError(t, svc.Merge().RollbackExperiment(ctx, "x"))
	node, err := svc.si.GraphNodeIDByBlobForTest(ctx, "kb/e.md", eBlob)
	require.NoError(t, err)
	require.NotZero(t, node, "precondition: the orphaned version keeps its node")

	var resets int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM meta WHERE key = 'derived_from_per_blob'`).Scan(&resets))
	require.Zero(t, resets, "precondition: this rebuild spends the v8 reset")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil)) // the v8 reset + drain + backfill
	var v string
	require.NoError(t, db.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key = 'ref_edges_backfilled'), '')`).Scan(&v))
	require.Equal(t, "1", v, "the rebuild set the sentinel again: readers use the table")
	require.Equal(t, want, rowsOfBlob(allRefRows(t, db), eBlob), "the backfill restored the orphan's cross-KB row")
}

// PR #694 review round 2, R2-F1: the specs and rowsOK come from ONE repo-id
// lookup. With the first k lookups failing and the rest succeeding (the
// retry), a write of G citing kb://<self>/kb/r.md never stores that ref as a
// cross-KB row naming this repo (whatever k: the reviewer's probe, ported).
//
// Sabotage: decide rowsOK with a second localRepoID call again, and k=3 stores
// the self-repo cross-KB row.
func TestRefEdges_SpecsAndRowsUseOneRepoIDLookup(t *testing.T) {
	ctx := context.Background()
	for k := 1; k <= 4; k++ {
		t.Run(fmt.Sprintf("fail-first-%d-lookups", k), func(t *testing.T) {
			svc := newMergeTestStore(t)
			db := svc.si.RepoForTest().DB()
			write(t, svc, "main", "kb/r.md", "R")
			root, err := svc.si.RepoForTest().RootCommit(ctx, "main")
			require.NoError(t, err)
			self := fact.ID12(root)
			real := svc.si.RepoForTest().RootCommit
			calls := 0
			svc.si.SetRootCommitForTest(func(ctx context.Context, b string) (string, error) {
				calls++
				if calls <= k {
					return "", errors.New("transient")
				}
				return real(ctx, b)
			})
			_, gBlob := write(t, svc, "main", "kb/g.md", "G", "kb://"+self+"/kb/r.md")
			require.Greater(t, calls, 0, "precondition: the lookup was reached")
			for _, r := range rowsOfBlob(allRefRows(t, db), gBlob) {
				require.NotEqual(t, self, r.Repo, "a kb://self ref is never stored as a cross-KB row naming this repo")
			}
		})
	}
}

// PR #694 review round 2: the edge fallback blast-radius walk (used while the
// sentinel is unset) has its recursive step pinned: the plan probes the
// frontier's nodes by blob instead of scanning every DERIVED_FROM edge per
// row; and on a deep chain it matches the table walk.
//
// Sabotage: unpin the recursive step (plain JOINs), and the plan uses
// idx_edges_type.
func TestRefEdgesReaders_EdgeFallbackWalkIsPinned(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	prev := "kb/c00.md"
	write(t, svc, "main", prev, "C0")
	for i := 1; i <= 25; i++ {
		p := fmt.Sprintf("kb/c%02d.md", i)
		write(t, svc, "main", p, fmt.Sprintf("C%d", i), prev)
		prev = p
	}
	plan, err := svc.gq.ReverseDependentsPlanForTest(ctx, true)
	require.NoError(t, err)
	require.NotContains(t, plan, "idx_edges_type", "the edge walk does not scan every edge per frontier row:\n%s", plan)
	require.Contains(t, plan, "SCAN d", "the recursive step starts from the frontier:\n%s", plan)

	table, err := svc.gq.BlastRadius(ctx, "main", "kb/c00.md")
	require.NoError(t, err)
	require.Equal(t, 25, table, "precondition: the chain's 25 dependents")
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	edges, err := svc.gq.BlastRadius(ctx, "main", "kb/c00.md")
	require.NoError(t, err)
	require.Equal(t, table, edges, "the fallback walk matches the table walk")
}

// PR #694 review round 2: an explicit full Rebuild spends the v8 reset and
// must leave the store backfilled (sentinel set, rows present, readers on the
// table), not on the fallback until some later heal.
//
// Sabotage: drop the backfill at the end of rebuildGraph, and the sentinel
// stays unset.
func TestRefEdges_FullRebuildLeavesTheStoreBackfilled(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	cF, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	var v string
	require.NoError(t, db.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key = 'ref_edges_backfilled'), '')`).Scan(&v))
	require.Equal(t, "1", v, "the sentinel is set after the rebuild")
	require.Len(t, rowsOfBlob(allRefRows(t, db), fBlob), 1, "the rows are there")
	// Readers use the table: with the edges gone (and the sentinel set), the
	// outgoing read still answers from the rows.
	_, err := db.Exec(`DELETE FROM edges WHERE type = 'DERIVED_FROM'`)
	require.NoError(t, err)
	require.Equal(t, []string{"kb/r.md"}, outOrder(t, svc, "main", "kb/f.md", cF), "outgoing reads the table")
}

// PR #694 review round 2: RefEdgesOfBlobs before the backfill with the repo id
// unresolved falls back (as outgoing does) instead of failing: kb://<self>/
// refs are recognised by their edges, in the author's order.
//
// Sabotage: return an error again when the repo id is unresolved, or skip
// selfSpecsFromEdges (the self ref comes back as a cross-KB row).
func TestRefEdges_LevelReadFallsBackWithoutTheRepoID(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/b.md", "B")
	write(t, svc, "main", "kb/r.md", "R")
	root, err := svc.si.RepoForTest().RootCommit(ctx, "main")
	require.NoError(t, err)
	self := fact.ID12(root)
	c, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb://0123456789ab/kb/far.md", "kb://"+self+"/kb/r.md", "kb/b.md")
	want, err := svc.gq.RefEdgesOfBlobs(ctx, []string{fBlob})
	require.NoError(t, err)
	require.Len(t, want, 3, "precondition: far (cross-KB), r (local, via kb://self), b")
	require.Equal(t, "", want[1].Repo)
	wantOut := outOrder(t, svc, "main", "kb/f.md", c)

	_, err = db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	svc.gq.SetRootCommitForTest(func(context.Context, string) (string, error) { return "", errors.New("unresolved") })
	defer svc.gq.SetRootCommitForTest(nil)
	got, err := svc.gq.RefEdgesOfBlobs(ctx, []string{fBlob})
	require.NoError(t, err, "no error without the repo id")
	require.Equal(t, want, got, "the same rows, in the author's order")
	require.Equal(t, wantOut, outOrder(t, svc, "main", "kb/f.md", c), "outgoing too")
}

// PR #694 review R3-N2: the schema enforces that a cross-KB row pins no
// version (the incoming read and the reverse walk rely on it): a cross-KB
// insert carrying a target_blob or a target_commit is rejected; a local row
// with a pin, and a cross-KB row without one, are accepted.
//
// Sabotage: drop the CHECK from migration 000035, and the bad insert lands.
func TestRefEdges_SchemaRejectsAPinnedCrossKBRow(t *testing.T) {
	svc := newMergeTestStore(t)
	db := svc.si.RepoForTest().DB()
	ins := func(ord int, repo string, blob, commit any) error {
		_, err := db.Exec(`INSERT INTO ref_edges(source_blob, ord, target_repo, target_path, target_blob, target_commit) VALUES ('b', ?, ?, 'kb/x.md', ?, ?)`,
			ord, repo, blob, commit)
		return err
	}
	require.NoError(t, ins(0, "", "tb", "tc"), "a local row with its pin")
	require.NoError(t, ins(1, "0123456789ab", nil, nil), "a cross-KB row without a pin")
	err := ins(2, "0123456789ab", "tb", nil)
	require.Error(t, err, "a cross-KB row with a target_blob is rejected")
	require.Contains(t, err.Error(), "CHECK")
	require.Error(t, ins(3, "0123456789ab", nil, "tc"), "a cross-KB row with a target_commit is rejected")
}

// PR #694 review R3-N1 (the reviewer's probe, ported, plus the second case):
// the fallback level read with the repo id unresolved learns this repo's id
// only from a kb://R/p ref whose edge cannot be an explicit local ref's.
//   - same-path-other-kb: F cites [kb://<other>/kb/r.md, kb/r.md]. The edge
//     to kb/r.md is the local ref's, so <other> is NOT taken for this repo:
//     the cross-KB row stays at ord 0 and the local row at ord 1, as in the
//     table.
//   - dangling-self-ref: F cites [kb://<self>/kb/r.md, kb://<self>/kb/g.md]
//     and the edge to g was dropped as dangling. The table (id known) has no
//     row for g (a local ref without an edge has none); the fallback learns
//     <self> from r's edge and gives no row for g either, never a cross-KB
//     row naming this repo.
//
// Sabotage: convert every cross-KB spec whose path has an edge (the round-2
// rule), and same-path-other-kb loses the other KB's row; keep a kb://self
// ref without an edge as cross-KB, and dangling-self-ref grows a row.
func TestRefEdges_FallbackLearnsTheSelfRepoID(t *testing.T) {
	ctx := context.Background()
	fallback := func(t *testing.T, svc *Service, blob string) []index.RefEdgeRow {
		db := svc.si.RepoForTest().DB()
		_, err := db.Exec(`DELETE FROM ref_edges`)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
		require.NoError(t, err)
		svc.gq.SetRootCommitForTest(func(context.Context, string) (string, error) { return "", errors.New("unavailable") })
		t.Cleanup(func() { svc.gq.SetRootCommitForTest(nil) })
		rows, err := svc.gq.RefEdgesOfBlobs(ctx, []string{blob})
		require.NoError(t, err)
		return rows
	}
	t.Run("same-path-other-kb", func(t *testing.T) {
		svc := newMergeTestStore(t)
		write(t, svc, "main", "kb/r.md", "R")
		_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb://0123456789ab/kb/r.md", "kb/r.md")
		table, err := svc.gq.RefEdgesOfBlobs(ctx, []string{fBlob})
		require.NoError(t, err)
		require.Len(t, table, 2, "precondition: the cross-KB row and the local row")
		require.Equal(t, "0123456789ab", table[0].Repo)
		require.Equal(t, table, fallback(t, svc, fBlob), "the fallback equals the table")
	})
	t.Run("dangling-self-ref", func(t *testing.T) {
		svc := newMergeTestStore(t)
		db := svc.si.RepoForTest().DB()
		write(t, svc, "main", "kb/r.md", "R")
		_, gBlob := write(t, svc, "main", "kb/g.md", "G")
		root, err := svc.si.RepoForTest().RootCommit(ctx, "main")
		require.NoError(t, err)
		self := fact.ID12(root)
		_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb://"+self+"/kb/r.md", "kb://"+self+"/kb/g.md")
		// g's blob is lost and F's edge to it dropped as dangling (as
		// dropDanglingRefEdges does in a rebuild); the repair rewrites F's
		// rows from its edges with the id known: the table's answer.
		_, err = db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, gBlob)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM edges WHERE id IN (SELECT e.id FROM edges e
			JOIN node_props_text tb ON tb.node_id = e.target_id AND tb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
			WHERE e.type = 'DERIVED_FROM' AND tb.value = ?)`, gBlob)
		require.NoError(t, err)
		_, err = svc.si.RepairRefEdgesForTest(ctx, "main")
		require.NoError(t, err)
		table, err := svc.gq.RefEdgesOfBlobs(ctx, []string{fBlob})
		require.NoError(t, err)
		require.Len(t, table, 1, "precondition: the table has the r row only")
		require.Equal(t, "kb/r.md", table[0].Path)
		got := fallback(t, svc, fBlob)
		require.Equal(t, table, got, "the fallback equals the table: no row for the dangling kb://self ref")
	})
}

// The fallback learns the self repo id over a BATCH from the blobs with
// exactly one candidate, and applies it to every blob of the batch (PR #694
// delta review D1/D2). An ambiguous blob alone converts nothing.
func TestRefEdges_FallbackLearnsTheSelfRepoIDOverTheBatch(t *testing.T) {
	ctx := context.Background()
	const foreign = "0123456789ab"
	// unbackfilled empties the table and drops the sentinel, so reads go
	// through the edge fallback, and makes the repo id unresolvable.
	unbackfilled := func(t *testing.T, svc *Service) {
		db := svc.si.RepoForTest().DB()
		_, err := db.Exec(`DELETE FROM ref_edges`)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
		require.NoError(t, err)
		svc.gq.SetRootCommitForTest(func(context.Context, string) (string, error) { return "", errors.New("unavailable") })
		t.Cleanup(func() { svc.gq.SetRootCommitForTest(nil) })
	}
	read := func(t *testing.T, svc *Service, blobs ...string) []index.RefEdgeRow {
		rows, err := svc.gq.RefEdgesOfBlobs(ctx, blobs)
		require.NoError(t, err)
		return rows
	}
	of := func(rows []index.RefEdgeRow, blob string) []index.RefEdgeRow {
		var out []index.RefEdgeRow
		for _, r := range rows {
			if r.SourceBlob == blob {
				out = append(out, r)
			}
		}
		return out
	}
	selfOf := func(t *testing.T, svc *Service) string {
		root, err := svc.si.RepoForTest().RootCommit(ctx, "main")
		require.NoError(t, err)
		return fact.ID12(root)
	}

	// (a) A's kb://self ref with an edge teaches the id to C, which has no
	// candidate of its own (its only kb://self ref lost its edge as
	// dangling): C gets no row, as in the table, instead of a cross-KB row
	// naming this repo.
	t.Run("batch-teaches-a-blob-with-no-candidate", func(t *testing.T) {
		svc := newMergeTestStore(t)
		db := svc.si.RepoForTest().DB()
		_, rBlob := write(t, svc, "main", "kb/r.md", "R")
		_, gBlob := write(t, svc, "main", "kb/g.md", "G")
		self := selfOf(t, svc)
		_, aBlob := write(t, svc, "main", "kb/a.md", "A", "kb://"+self+"/kb/r.md")
		_, cBlob := write(t, svc, "main", "kb/c.md", "C", "kb://"+self+"/kb/g.md")
		_, err := db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, gBlob)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM edges WHERE id IN (SELECT e.id FROM edges e
			JOIN node_props_text tb ON tb.node_id = e.target_id AND tb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
			WHERE e.type = 'DERIVED_FROM' AND tb.value = ?)`, gBlob)
		require.NoError(t, err)
		_, err = svc.si.RepairRefEdgesForTest(ctx, "main")
		require.NoError(t, err)
		table := read(t, svc, aBlob, cBlob)
		require.Len(t, table, 1, "precondition: A's local r row only, none for C")
		require.Equal(t, aBlob, table[0].SourceBlob)
		require.Equal(t, "", table[0].Repo)
		require.Equal(t, rBlob, table[0].TargetBlob)

		unbackfilled(t, svc)
		alone := read(t, svc, cBlob)
		require.Len(t, alone, 1, "precondition: C alone has no candidate and keeps its ref cross-KB")
		require.Equal(t, self, alone[0].Repo)

		got := read(t, svc, aBlob, cBlob)
		require.Empty(t, of(got, cBlob), "in the batch C learns the id from A: no row for its dangling kb://self ref")
		require.Equal(t, table, got, "the batch fallback equals the table")
	})

	// (b) B alone has two candidates (the foreign repo and self both cite
	// kb/r.md, which B has an edge to and no explicit local ref for): it is
	// ambiguous, so nothing is converted and both refs read as cross-KB.
	t.Run("an-ambiguous-blob-alone-converts-nothing", func(t *testing.T) {
		svc := newMergeTestStore(t)
		write(t, svc, "main", "kb/r.md", "R")
		self := selfOf(t, svc)
		_, bBlob := write(t, svc, "main", "kb/b.md", "B", "kb://"+foreign+"/kb/r.md", "kb://"+self+"/kb/r.md")
		unbackfilled(t, svc)
		got := read(t, svc, bBlob)
		require.Equal(t, []index.RefEdgeRow{
			{SourceBlob: bBlob, Ord: 0, Repo: foreign, Path: "kb/r.md"},
			{SourceBlob: bBlob, Ord: 1, Repo: self, Path: "kb/r.md"},
		}, got, "neither candidate is picked: both refs stay cross-KB, unpinned")
	})

	// (c) The D1 batch: A (one candidate, self) and the ambiguous B. A does
	// the learning, and B gets self applied: ord 0 stays the foreign cross-KB
	// row, ord 1 is the local pinned row, as in the table.
	t.Run("an-ambiguous-blob-does-not-switch-batch-learning-off", func(t *testing.T) {
		svc := newMergeTestStore(t)
		rCommit, rBlob := write(t, svc, "main", "kb/r.md", "R")
		self := selfOf(t, svc)
		_, aBlob := write(t, svc, "main", "kb/a.md", "A", "kb://"+self+"/kb/r.md")
		_, bBlob := write(t, svc, "main", "kb/b.md", "B", "kb://"+foreign+"/kb/r.md", "kb://"+self+"/kb/r.md")
		table := read(t, svc, aBlob, bBlob)
		require.Equal(t, []index.RefEdgeRow{
			{SourceBlob: bBlob, Ord: 0, Repo: foreign, Path: "kb/r.md"},
			{SourceBlob: bBlob, Ord: 1, Repo: "", Path: "kb/r.md", TargetBlob: rBlob, TargetCommit: rCommit},
		}, of(table, bBlob), "precondition: the table's rows for B")

		unbackfilled(t, svc)
		got := read(t, svc, aBlob, bBlob)
		require.Equal(t, []index.RefEdgeRow{
			{SourceBlob: bBlob, Ord: 0, Repo: foreign, Path: "kb/r.md"},
			{SourceBlob: bBlob, Ord: 1, Repo: "", Path: "kb/r.md", TargetBlob: rBlob, TargetCommit: rCommit},
		}, of(got, bBlob), "B's ord 1 comes back as the local pinned row")
		require.Equal(t, table, got, "the batch fallback equals the table")
	})
}
