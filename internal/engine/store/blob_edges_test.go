package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/fsck"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/merge"
)

// Reference edges are per BLOB and written ONCE (user ruling 2026-10-30): a
// blob's refs become DERIVED_FROM edges when the blob is first seen, resolved
// at the commit that introduced it; the blob arriving anywhere else adds
// nothing.

// refEdge is one DERIVED_FROM edge as these tests compare it.
type refEdge struct {
	SrcPath, SrcBlob, TgtPath, TgtBlob, SourceCommit, TargetCommit string
}

func allRefEdges(t *testing.T, db *sql.DB) []refEdge {
	t.Helper()
	rows, err := db.Query(`
		SELECT sp.value, sb.value, tp.value, tb.value, COALESCE(sc.value, ''), COALESCE(tc.value, '')
		  FROM edges e
		  JOIN node_props_text sp ON sp.node_id = e.source_id AND sp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
		  JOIN node_props_text sb ON sb.node_id = e.source_id AND sb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
		  JOIN node_props_text tp ON tp.node_id = e.target_id AND tp.key_id = (SELECT id FROM property_keys WHERE key = 'path')
		  JOIN node_props_text tb ON tb.node_id = e.target_id AND tb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash')
		  LEFT JOIN edge_props_text sc ON sc.edge_id = e.id AND sc.key_id = (SELECT id FROM property_keys WHERE key = 'source_commit')
		  LEFT JOIN edge_props_text tc ON tc.edge_id = e.id AND tc.key_id = (SELECT id FROM property_keys WHERE key = 'target_commit')
		 WHERE e.type = 'DERIVED_FROM'`)
	require.NoError(t, err)
	defer rows.Close()
	var out []refEdge
	for rows.Next() {
		var e refEdge
		require.NoError(t, rows.Scan(&e.SrcPath, &e.SrcBlob, &e.TgtPath, &e.TgtBlob, &e.SourceCommit, &e.TargetCommit))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SrcBlob != b.SrcBlob {
			return a.SrcBlob < b.SrcBlob
		}
		return a.TgtPath < b.TgtPath
	})
	return out
}

// edgesOfBlob is the edges whose source is the blob, as (target path ->
// target blob).
func edgesOfBlob(edges []refEdge, blob string) map[string]string {
	out := map[string]string{}
	for _, e := range edges {
		if e.SrcBlob == blob {
			out[e.TgtPath] = e.TgtBlob
		}
	}
	return out
}

func write(t *testing.T, svc *Service, branch, path, title string, refs ...string) (commit, blob string) {
	t.Helper()
	r, err := svc.Facts().WriteFact(context.Background(), branch, path, testFactBody(title, 0.8, refs), "w "+path, "")
	require.NoError(t, err)
	return r.CommitHash, r.BlobHash
}

func outPaths(t *testing.T, svc *Service, branch, path, commit string) map[string]string {
	t.Helper()
	out, err := svc.gq.OutgoingAtCommit(context.Background(), branch, path, commit)
	require.NoError(t, err)
	m := map[string]string{}
	for _, o := range out {
		_, dup := m[o.Path]
		require.False(t, dup, "%s: one row per cited path", o.Path)
		m[o.Path] = o.Commit
	}
	return m
}

// The user's example: F@c1 -> R@c1; R becomes R1 at c2: F's edges do not
// change; F becomes F1 at c3: the new node F1 -> R1@c2, and F still -> R@c1.
//
// Sabotage: let a COW hit or a later arrival re-resolve (claimBlobRefs
// returns true for any arrival), and the R re-pin appears.
func TestBlobEdges_UserExample(t *testing.T) {
	svc := newMergeTestStore(t)
	db := svc.si.RepoForTest().DB()
	c1, rBlob := write(t, svc, "main", "kb/r.md", "R")
	cF, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.Equal(t, map[string]string{"kb/r.md": c1}, outPaths(t, svc, "main", "kb/f.md", cF))

	c2, r1Blob := write(t, svc, "main", "kb/r.md", "R1")
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(allRefEdges(t, db), fBlob), "R changing touches nothing")
	require.Equal(t, map[string]string{"kb/r.md": c1}, outPaths(t, svc, "main", "kb/f.md", c2))

	c3, f1Blob := write(t, svc, "main", "kb/f.md", "F1", "kb/r.md")
	edges := allRefEdges(t, db)
	require.Equal(t, map[string]string{"kb/r.md": r1Blob}, edgesOfBlob(edges, f1Blob), "F1 -> R1")
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(edges, fBlob), "F -> R, unchanged")
	require.Equal(t, map[string]string{"kb/r.md": c2}, outPaths(t, svc, "main", "kb/f.md", c3))
	require.Len(t, edges, 2)
}

// A merge carrying F unchanged adds no edge, even though R moved on the
// destination before the merge; F still pins R as of F's own commit. A
// branch copy and its re-index add nothing either.
//
// Sabotage: re-resolve on every COW hit (the old behaviour), and the merge
// adds F -> R2.
func TestBlobEdges_MergeCarryingABlobAddsNothing(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	cF, fBlob := write(t, svc, "feature", "kb/f.md", "F", "kb/r.md")
	write(t, svc, "main", "kb/r.md", "R2")
	before := allRefEdges(t, db)

	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "main", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	require.Equal(t, before, allRefEdges(t, db), "the merge carried F unchanged: no edge added")
	head, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	got := outPaths(t, svc, "main", "kb/f.md", head)
	require.Len(t, got, 1)
	require.NotEmpty(t, edgesOfBlob(before, fBlob))
	_ = cF

	require.NoError(t, svc.CreateBranch(ctx, "copy", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "copy"))
	require.Equal(t, before, allRefEdges(t, db), "a branch copy adds nothing")
}

// A merge that RESOLVES a conflict in F writes a new blob at the merge
// commit: a new node, whose refs are resolved at the merge commit (the
// destination's R2 at that point).
func TestBlobEdges_ConflictResolvingMergeAddsNewEdges(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	write(t, svc, "feature", "kb/f.md", "F feature", "kb/r.md")
	write(t, svc, "main", "kb/f.md", "F main", "kb/r.md")
	_, r2Blob := write(t, svc, "main", "kb/r.md", "R2")

	merged := []byte(testFactBody("F merged", 0.8, []string{"kb/r.md"}))
	_, err := svc.Merge().MergeIntoBranchLockedOpts(ctx, "feature", "main", merge.StrategyRefuse,
		map[string]merge.Resolution{"kb/f.md": {Body: merged}}, merge.MergeOpts{})
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	head, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	blob, err := svc.si.RepoForTest().ReadBlobHashAtCommit(ctx, "kb/f.md", head)
	require.NoError(t, err)
	edges := allRefEdges(t, db)
	require.Equal(t, map[string]string{"kb/r.md": r2Blob}, edgesOfBlob(edges, blob), "the merged blob pins R as of the merge")
	for _, e := range edges {
		if e.SrcBlob == blob {
			at, err := svc.si.RepoForTest().ReadBlobHashAtCommit(ctx, "kb/f.md", e.SourceCommit)
			require.NoError(t, err)
			require.Equal(t, blob, at, "resolved at a commit of the merge that holds the merged blob")
		}
	}
}

// Rename (same bytes at a new path) and retract-then-restore add no edge;
// the renamed path reads the blob's edges.
func TestBlobEdges_RenameAndRestoreKeepEdges(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	c1, _ := write(t, svc, "main", "kb/r.md", "R")
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	write(t, svc, "main", "kb/r.md", "R2")
	before := allRefEdges(t, db)

	commit, blobs, err := svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{"kb/g.md": testFactBody("F", 0.8, []string{"kb/r.md"})},
		[]string{"kb/f.md"}, "rename f -> g", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	require.Equal(t, fBlob, blobs["kb/g.md"], "precondition: the same blob at a new path")
	require.Equal(t, before, allRefEdges(t, db), "a rename adds nothing")
	require.Equal(t, map[string]string{"kb/r.md": c1}, outPaths(t, svc, "main", "kb/g.md", commit), "the renamed path reads the blob's edges")

	_, err = svc.Facts().DeleteFact(ctx, "main", "kb/g.md", "retract")
	require.NoError(t, err)
	cR, _ := write(t, svc, "main", "kb/g.md", "F", "kb/r.md")
	require.Equal(t, before, allRefEdges(t, db), "restore adds nothing")
	require.Equal(t, map[string]string{"kb/r.md": c1}, outPaths(t, svc, "main", "kb/g.md", cR))
}

// An experiment's commit (merged into the agent branch) adds nothing for a
// blob it carries; a blob first written IN the experiment keeps the edges
// it was given there.
func TestBlobEdges_ExperimentCommitAddsNothing(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	_, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	write(t, svc, "agent/test", "kb/r.md", "R2")
	before := allRefEdges(t, db)
	require.NotEmpty(t, edgesOfBlob(before, fBlob), "precondition: F's edges were written in the experiment")
	cr, err := svc.Merge().CommitExperiment(ctx, "x", nil)
	require.NoError(t, err)
	require.Equal(t, merge.ModeMerge, cr.Mode, "precondition: a merge commit")
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	require.Equal(t, before, allRefEdges(t, db), "the experiment commit adds nothing")
}

// Identical bytes written independently on two branches, whose ref resolves
// to different target versions on each: the blob's edges are resolved at the
// EARLIEST introduction, whichever branch is indexed first, so the result
// does not depend on indexing order; rebuilding every branch from scratch
// gives the same edges as the incremental index.
//
// Sabotage: keep the first claim even when a strictly earlier introduction
// arrives later, and the two orders disagree.
func TestBlobEdges_IdenticalBytesOnTwoBranchesAndRebuild(t *testing.T) {
	run := func(t *testing.T, laterFirst bool) []refEdge {
		svc := newMergeTestStore(t)
		ctx := context.Background()
		db := svc.si.RepoForTest().DB()
		write(t, svc, "main", "kb/r.md", "R")
		for _, b := range []string{"b1", "b2"} {
			require.NoError(t, svc.CreateBranch(ctx, b, "main"))
			require.NoError(t, svc.IndexManager().Sync(ctx, b))
		}
		write(t, svc, "b1", "kb/r.md", "R on b1")
		write(t, svc, "b2", "kb/r.md", "R on b2")
		// F, byte-identical, on both branches; b1's commit is the earlier.
		write(t, svc, "b1", "kb/f.md", "F", "kb/r.md")
		write(t, svc, "b2", "kb/f.md", "F", "kb/r.md")
		if laterFirst {
			// Wipe the reference edges and re-index b2 (the later
			// introduction) before b1: the earlier one must still win.
			resetRefEdgesForTest(t, db)
			require.NoError(t, svc.IndexManager().Rebuild(ctx, "b2", nil))
			require.NoError(t, svc.IndexManager().Rebuild(ctx, "b1", nil))
			require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
		}
		incremental := allRefEdges(t, db)
		resetRefEdgesForTest(t, db)
		for _, b := range []string{"main", "b1", "b2"} {
			require.NoError(t, svc.IndexManager().Rebuild(ctx, b, nil))
		}
		require.Equal(t, incremental, allRefEdges(t, db), "rebuild == incremental")
		return incremental
	}
	a := run(t, false)
	b := run(t, true)
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.Equal(t, a[0].TgtPath, b[0].TgtPath)
	require.Equal(t, a[0].SrcBlob, b[0].SrcBlob)
}

// resetRefEdgesForTest drops every DERIVED_FROM edge and refs mark.
func resetRefEdgesForTest(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`DELETE FROM edges WHERE type = 'DERIVED_FROM'`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM node_props_text WHERE key_id = (SELECT id FROM property_keys WHERE key = 'refs_commit')`)
	require.NoError(t, err)
}

// Every reader shows the same references for a version: Connections and
// materialize (OutgoingAtCommit), explain (ExplainFact), and incoming is its
// mirror.
func TestBlobEdges_ReadersAgree(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/s.md", "S")
	c, _ := write(t, svc, "main", "kb/f.md", "F", "kb/r.md", "kb/s.md")
	write(t, svc, "main", "kb/r.md", "R2")
	out, err := svc.gq.OutgoingAtCommit(ctx, "main", "kb/f.md", c)
	require.NoError(t, err)
	ex, err := svc.gq.ExplainFact(ctx, "main", "kb/f.md")
	require.NoError(t, err)
	require.Equal(t, out, ex.Outgoing)
	for _, o := range out {
		in, err := svc.gq.IncomingAtCommit(ctx, "main", o.Path, o.Commit)
		require.NoError(t, err)
		require.Len(t, in, 1, "%s: F cites it once", o.Path)
		require.Equal(t, "kb/f.md", in[0].Path)
	}
}

// A reference edge joins two REAL blobs: a target whose blob is not in the
// object store gets no edge, on the write path and in the rebuild, and the
// skip is counted.
//
// Sabotage: drop the target-blob check, and the edge to r is written.
func TestBlobEdges_NoEdgeToAMissingBlob(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, rBlob := write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/s.md", "S")
	_, err := db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, rBlob)
	require.NoError(t, err)
	before := svc.si.DanglingRefsSkipped()
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md", "kb/s.md")
	got := edgesOfBlob(allRefEdges(t, db), fBlob)
	require.NotContains(t, got, "kb/r.md", "no edge to a blob that is not in the object store")
	require.Contains(t, got, "kb/s.md", "the other ref still gets its edge")
	require.Greater(t, svc.si.DanglingRefsSkipped(), before, "the skip is counted")

	// The rebuild's re-resolve checks too: G pins r's FIRST version, whose
	// blob is then lost while r's tip version stays (a rebuild needs the
	// tip's blobs to list the tree).
	svc2 := newMergeTestStore(t)
	db2 := svc2.si.RepoForTest().DB()
	_, r1 := write(t, svc2, "main", "kb/r.md", "R")
	write(t, svc2, "main", "kb/s.md", "S")
	_, gBlob := write(t, svc2, "main", "kb/g.md", "G", "kb/r.md", "kb/s.md")
	write(t, svc2, "main", "kb/r.md", "R2")
	require.Contains(t, edgesOfBlob(allRefEdges(t, db2), gBlob), "kb/r.md", "precondition: G pins r v1")
	_, err = db2.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, r1)
	require.NoError(t, err)
	resetRefEdgesForTest(t, db2)
	require.NoError(t, svc2.IndexManager().Rebuild(ctx, "main", nil))
	got = edgesOfBlob(allRefEdges(t, db2), gBlob)
	require.NotContains(t, got, "kb/r.md", "the rebuild writes no edge to the lost blob")
	require.Contains(t, got, "kb/s.md")
}

// A version whose OWN blob is not in the object store gets no reference
// edges, and the skip is counted.
//
// Sabotage: drop the source-blob check, and F's edge to r is written.
func TestBlobEdges_NoEdgesFromAMissingSourceBlob(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	c, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	resetRefEdgesForTest(t, db)
	_, err := db.Exec(`DELETE FROM objects WHERE hash = ? AND type = 3`, fBlob)
	require.NoError(t, err)
	before := svc.si.DanglingRefsSkipped()
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", fBlob, c, []string{"kb/r.md"}))
	require.Empty(t, allRefEdges(t, db), "no edges from a version whose blob is not in the object store")
	require.Greater(t, svc.si.DanglingRefsSkipped(), before)
}

// The order of a blob's introductions: an ancestor precedes its descendant
// even when both are in the same second and the descendant's hash sorts
// first (commit times are forced equal here, hashes compared both ways).
//
// Sabotage: break a same-second tie by hash alone, and one direction fails.
func TestBlobEdges_AncestorPrecedesDescendantOnATie(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	a, _ := write(t, svc, "main", "kb/a.md", "A")
	var d string
	for i := 0; i < 64 && (d == "" || d > a); i++ {
		d, _ = write(t, svc, "main", "kb/b.md", fmt.Sprintf("B %d", i))
	}
	require.Less(t, d, a, "precondition: the descendant's hash sorts first")
	_, err := db.Exec(`UPDATE commit_log SET committed_at = 1000 WHERE commit_hash IN (?, ?)`, a, d)
	require.NoError(t, err)
	early, err := svc.si.CommitEarlierForTest(ctx, a, d)
	require.NoError(t, err)
	require.True(t, early, "the ancestor precedes")
	late, err := svc.si.CommitEarlierForTest(ctx, d, a)
	require.NoError(t, err)
	require.False(t, late, "the descendant does not, whatever the hashes")
}

// The GraphSchemaVersion 8 upgrade: a store indexed by the old scheme holds a
// blob's edges re-pinned at a later commit (injected here as the COW-hit path
// used to write them); the first rebuild drops every reference edge once and
// re-resolves, leaving one pin per ref, and a second rebuild changes nothing.
//
// Sabotage: skip resetBlobRefsOnce, and the legacy re-pin survives.
func TestBlobEdges_UpgradeRebuildDropsAccumulatedEdges(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, rBlob := write(t, svc, "main", "kb/r.md", "R")
	_, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	c2, r2Blob := write(t, svc, "main", "kb/r.md", "R2")
	src, err := svc.si.GraphNodeIDByBlobForTest(ctx, "kb/f.md", fBlob)
	require.NoError(t, err)
	r2, err := svc.si.GraphNodeIDByBlobForTest(ctx, "kb/r.md", r2Blob)
	require.NoError(t, err)
	id, err := svc.si.GraphInsertEdgeReturningIDForTest(ctx, src, r2, index.EdgeDerivedFrom)
	require.NoError(t, err)
	require.NoError(t, svc.si.GraphSetEdgePropsForTest(ctx, id, map[string]string{"source_commit": c2, "target_commit": c2}))
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'derived_from_per_blob'`)
	require.NoError(t, err)
	require.Len(t, allRefEdges(t, db), 2, "precondition: the legacy re-pin")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	after := allRefEdges(t, db)
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(after, fBlob), "one pin: R as of F's commit")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, after, allRefEdges(t, db), "a second rebuild changes nothing")
}

// Verify reports a reference edge to a blob that is not in the object store
// (injected), and a rebuild drops it.
//
// Sabotage: drop checkReferenceBlobs from Verify, and nothing is reported.
func TestBlobEdges_VerifyReportsAndRebuildDropsDanglingEdges(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	_, fBlob := write(t, svc, "main", "kb/f.md", "F")
	count := func() int {
		rep, err := svc.Integrity().Verify(ctx, fsck.VerifyOpts{})
		require.NoError(t, err)
		n := 0
		for _, i := range rep.Issues {
			if i.Category == fsck.CategoryReferenceBlobs {
				n++
			}
		}
		return n
	}
	require.Zero(t, count())
	// A first rebuild spends the one-time v8 reset, so the drop below is the
	// dangling-edge drop's, not the reset's.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	src, err := svc.si.GraphNodeIDByBlobForTest(ctx, "kb/f.md", fBlob)
	require.NoError(t, err)
	ghost, err := index.GraphMergeNodeForTest(ctx, db, index.NodeFact, map[string]string{"path": "kb/ghost.md", "blob_hash": "0123456789abcdef0123456789abcdef01234567"})
	require.NoError(t, err)
	require.NoError(t, svc.si.GraphInsertEdgeForTest(ctx, src, ghost, index.EdgeDerivedFrom))
	n, err := svc.si.DanglingRefEdges(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.Equal(t, 1, count())

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	n, err = svc.si.DanglingRefEdges(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "the rebuild drops it")
}
