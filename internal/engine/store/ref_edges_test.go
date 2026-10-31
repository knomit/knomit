package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

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

	// A version citing ONLY another KB has no edge but still gets its row.
	_, gBlob := write(t, svc, "main", "kb/g.md", "G", "kb://0123456789ab/kb/far.md")
	require.Equal(t, []refRow{{Blob: gBlob, Ord: 0, Repo: "0123456789ab", Path: "kb/far.md"}}, rowsOfBlob(allRefRows(t, db), gBlob))
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
