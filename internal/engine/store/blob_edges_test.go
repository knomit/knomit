package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/facts"
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

// countEdgesOfBlob is how many reference edges leave the blob.
func countEdgesOfBlob(edges []refEdge, blob string) int {
	n := 0
	for _, e := range edges {
		if e.SrcBlob == blob {
			n++
		}
	}
	return n
}

// The user's example: F@c1 -> R@c1; R becomes R1 at c2: F's edges do not
// change; F becomes F1 at c3: the new node F1 -> R1@c2, and F still -> R@c1.
//
// The same bytes then arrive at ANOTHER path, kb/h.md at ch (a fact's bytes
// do not depend on its path). introducingCommit returns ch, h's own
// introduction, so only the claim check (F is claimed at cF, which is
// earlier) stops the re-pin. Sabotage: let claimBlobRefs's refusal through
// (graphAddDerivedFromAtCommitTx ignores !write), and the blob re-pins to R1.
func TestBlobEdges_UserExample(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	c1, rBlob := write(t, svc, "main", "kb/r.md", "R")
	cF, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.Equal(t, map[string]string{"kb/r.md": c1}, outPaths(t, svc, "main", "kb/f.md", cF))

	c2, r1Blob := write(t, svc, "main", "kb/r.md", "R1")
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(allRefEdges(t, db), fBlob), "R changing touches nothing")
	require.Equal(t, map[string]string{"kb/r.md": c1}, outPaths(t, svc, "main", "kb/f.md", c2))

	ch, hBlob := write(t, svc, "main", "kb/h.md", "F", "kb/r.md")
	require.Equal(t, fBlob, hBlob, "precondition: the same bytes at another path")
	at, err := svc.si.IntroducingCommitForTest(ctx, "main", "kb/h.md", fBlob, ch)
	require.NoError(t, err)
	require.Equal(t, ch, at, "precondition: the arrival resolves to h's own commit, so the claim decides")
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/h.md", fBlob, ch, []string{"kb/r.md"}))
	edges := allRefEdges(t, db)
	require.Equal(t, 1, countEdgesOfBlob(edges, fBlob), "the claim refuses the blob's later arrival at kb/h.md")
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(edges, fBlob))
	require.Equal(t, cF, refsCommitOf(t, db, fBlob))

	c3, f1Blob := write(t, svc, "main", "kb/f.md", "F1", "kb/r.md")
	edges = allRefEdges(t, db)
	require.Equal(t, map[string]string{"kb/r.md": r1Blob}, edgesOfBlob(edges, f1Blob), "F1 -> R1")
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(edges, fBlob), "F -> R, unchanged")
	require.Equal(t, map[string]string{"kb/r.md": c2}, outPaths(t, svc, "main", "kb/f.md", c3))
	require.Len(t, edges, 2)
}

// A merge carrying F unchanged adds no edge, even though R moved on the
// destination before the merge; F still pins R as of F's own commit. A
// branch copy and its re-index add nothing either.
//
// The merge's own arrival resolves to F's own commit (introducingCommit takes
// the earliest introduction at kb/f.md), so it only re-fills. The same bytes
// then arrive at another path on main, kb/h.md, whose own introduction is
// later than the claim: the claim decides. Sabotage: let claimBlobRefs's
// refusal through, and the blob re-pins to R2.
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
	at, err := svc.si.IntroducingCommitForTest(ctx, "main", "kb/f.md", fBlob, head)
	require.NoError(t, err)
	require.Equal(t, cF, at, "the merge's arrival resolves to F's own commit")
	ch, hBlob := write(t, svc, "main", "kb/h.md", "F", "kb/r.md")
	require.Equal(t, fBlob, hBlob, "precondition: the same bytes at another path")
	at, err = svc.si.IntroducingCommitForTest(ctx, "main", "kb/h.md", fBlob, ch)
	require.NoError(t, err)
	require.Equal(t, ch, at, "precondition: h's own commit, so the claim decides")
	require.Equal(t, before, allRefEdges(t, db), "the claim refuses the blob's arrival at kb/h.md")

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

// refsCommitOf is the blob's refs_commit claim ("" when unclaimed).
func refsCommitOf(t *testing.T, db *sql.DB, blob string) string {
	t.Helper()
	var at string
	err := db.QueryRow(`
		SELECT rc.value
		  FROM node_props_text nb
		  JOIN node_props_text rc ON rc.node_id = nb.node_id
		       AND rc.key_id = (SELECT id FROM property_keys WHERE key = 'refs_commit')
		 WHERE nb.key_id = (SELECT id FROM property_keys WHERE key = 'blob_hash') AND nb.value = ?`, blob).Scan(&at)
	if err == sql.ErrNoRows {
		return ""
	}
	require.NoError(t, err)
	return at
}

// pendingRefBranches lists the branches the v8 reset marked and that have
// not been re-resolved yet.
func pendingRefBranches(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT key FROM meta WHERE key LIKE 'derived_from_per_blob_pending:%' ORDER BY key`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		out = append(out, k)
	}
	require.NoError(t, rows.Err())
	return out
}

// B1 (review of #693): the v8 upgrade rebuilds only the maintained branch,
// but its reset drops EVERY branch's reference edges. A blob that lives only
// on an open experiment must get its edges back from that same rebuild, not
// stay edgeless until something re-indexes the experiment.
//
// Sabotage: skip reResolvePendingBranches in rebuildGraph, and F's outgoing
// on the experiment is empty after the upgrade.
func TestBlobEdges_UpgradeReResolvesEveryBranch(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	cF, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	require.Equal(t, map[string]string{"kb/r.md": cR}, outPaths(t, svc, "exp/x", "kb/f.md", cF), "precondition")

	// The first v8 boot: the sentinel is absent, the heal rebuilds the
	// maintained branch only.
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'derived_from_per_blob'`)
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
	require.Equal(t, map[string]string{"kb/r.md": cR}, outPaths(t, svc, "exp/x", "kb/f.md", cF), "the experiment-only blob is re-resolved by the upgrade rebuild")
	require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(allRefEdges(t, db), fBlob))
	require.Equal(t, cF, refsCommitOf(t, db, fBlob))
	require.Empty(t, pendingRefBranches(t, db), "every pending mark is drained")
}

// An upgrade interrupted after the reset committed (edges dropped, every
// branch marked pending) and before the drain finished: the next rebuild,
// which the un-bumped schema version forces, drains the marks and fills in.
//
// Sabotage: clear the marks in resetBlobRefsOnce's place without re-resolving
// (skip reResolvePendingBranches), and F stays edgeless.
func TestBlobEdges_InterruptedUpgradeDrainResumes(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, _ := write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	cF, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil)) // spends the reset
	require.Empty(t, pendingRefBranches(t, db))

	// The state a crash right after the reset's transaction leaves.
	resetRefEdgesForTest(t, db)
	_, err = db.Exec(`INSERT OR REPLACE INTO meta(key, value) SELECT 'derived_from_per_blob_pending:' || name, '1' FROM branches`)
	require.NoError(t, err)
	require.Len(t, pendingRefBranches(t, db), 3, "precondition: agent/test, main and exp/x pending")
	require.Empty(t, outPaths(t, svc, "exp/x", "kb/f.md", cF), "precondition: F has no edges")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
	require.Equal(t, map[string]string{"kb/r.md": cR}, outPaths(t, svc, "exp/x", "kb/f.md", cF))
	require.Equal(t, cF, refsCommitOf(t, db, fBlob))
	require.Empty(t, pendingRefBranches(t, db))
}

// B1, probe E of the review: after the upgrade, the experiment commits as a
// MERGE. The incremental index of the merge and a later rebuild must agree,
// and both must pin F's refs at F's own commit (R), never at the merge (R2).
//
// Sabotage: introducingCommit falls back to the tip's version when the passed
// commit does not hold the blob (and skip the upgrade's re-resolve of every
// branch), and the incremental pin is the merge's R2.
func TestBlobEdges_UpgradeThenMergeCommitRebuildEqualsIncremental(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, rBlob := write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	cF, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	write(t, svc, "agent/test", "kb/r.md", "R2") // the agent moves: the commit is a merge
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'derived_from_per_blob'`)
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))

	cr, err := svc.Merge().CommitExperiment(ctx, "x", nil)
	require.NoError(t, err)
	require.Equal(t, merge.ModeMerge, cr.Mode, "precondition: a merge commit")
	require.NoError(t, svc.IndexManager().Sync(ctx, "agent/test"))
	inc := allRefEdges(t, db)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
	reb := allRefEdges(t, db)
	require.Equal(t, inc, reb, "rebuild == incremental")
	require.Equal(t, 1, countEdgesOfBlob(inc, fBlob))
	for _, e := range inc {
		if e.SrcBlob == fBlob {
			require.Equal(t, rBlob, e.TgtBlob, "F pins the R it saw")
			require.Equal(t, cF, e.SourceCommit, "resolved at F's own commit")
			require.Equal(t, cR, e.TargetCommit)
		}
	}
}

// introducingCommit alone: given a commit that does not hold the blob (the
// LastCommitForPath misattribution, or a merge's unrelated commit), an
// unclaimed blob is resolved at the EARLIEST commit that introduced it, not
// at the branch tip's version (here the merge, which holds it too).
//
// Sabotage: restore the tip fallback ahead of earliestIntroduction, and the
// merge comes back.
func TestBlobEdges_IntroducingCommitIsTheEarliestHolder(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	cF, fBlob := write(t, svc, "feature", "kb/f.md", "F", "kb/r.md")
	cR2, _ := write(t, svc, "main", "kb/r.md", "R2")
	require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", "main", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	head, err := svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	rh := svc.si.RepoForTest()
	got, err := rh.ReadBlobHashAtCommit(ctx, "kb/f.md", head)
	require.NoError(t, err)
	require.Equal(t, fBlob, got, "precondition: the merge holds F")
	_, err = rh.ReadBlobHashAtCommit(ctx, "kb/f.md", cR2)
	require.Error(t, err, "precondition: the passed commit does not hold F")

	resetRefEdgesForTest(t, db) // unclaimed
	at, err := svc.si.IntroducingCommitForTest(ctx, "main", "kb/f.md", fBlob, cR2)
	require.NoError(t, err)
	require.Equal(t, cF, at, "the commit that wrote F, not the merge %s", head)

	// Claimed: the same answer (the claim then only re-fills).
	require.NoError(t, svc.IndexManager().Rebuild(ctx, "main", nil))
	require.Equal(t, cF, refsCommitOf(t, db, fBlob))
	at, err = svc.si.IntroducingCommitForTest(ctx, "main", "kb/f.md", fBlob, cR2)
	require.NoError(t, err)
	require.Equal(t, cF, at)
}

// B2 (review of #693; dev's behaviour): after a rename f -> g, R's incoming
// and explain list the live kb/g.md (and the retracted kb/f.md, as dev did),
// though the edge sits on f's node. A rename made on ANOTHER branch does not
// surface on this one.
//
// Sabotage: describe the incoming source by the edge's own node only, and
// kb/g.md is missing; drop the branch check for the blob's other nodes, and
// the other branch's kb/h.md shows up on main.
func TestBlobEdges_RenameIncomingListsTheLivePath(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	c1, _ := write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.CreateBranch(ctx, "side", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "side"))
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "side", map[string]string{"kb/h.md": testFactBody("F", 0.8, []string{"kb/r.md"})},
		[]string{"kb/f.md"}, "rename f -> h on side", "")
	require.NoError(t, err)
	_, _, err = svc.Facts().BatchWriteFacts(ctx, "main", map[string]string{"kb/g.md": testFactBody("F", 0.8, []string{"kb/r.md"})},
		[]string{"kb/f.md"}, "rename f -> g", "")
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))

	summarize := func(in []index.RefSummary) map[string]bool {
		m := map[string]bool{}
		for _, r := range in {
			_, dup := m[r.Path]
			require.False(t, dup, "%s listed once", r.Path)
			m[r.Path] = r.Deleted
		}
		return m
	}
	in, err := svc.gq.IncomingAtCommit(ctx, "main", "kb/r.md", c1)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"kb/f.md": true, "kb/g.md": false}, summarize(in), "path -> deleted")
	require.Equal(t, "kb/f.md", in[0].Path, "order: the edge's own node first")
	ex, err := svc.gq.ExplainFact(ctx, "main", "kb/r.md")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"kb/f.md": true, "kb/g.md": false}, summarize(ex.Incoming))

	in, err = svc.gq.IncomingAtCommit(ctx, "side", "kb/r.md", c1)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"kb/f.md": true, "kb/h.md": false}, summarize(in), "side sees its own rename, not main's")
}

// m2 (review of #693): three same-second introductions of one blob, A and B
// on main (A an ancestor of B, B re-writing A's bytes) and C on an unrelated
// branch, with hashes B < C < A. The old pairwise order (ancestor, else hash)
// is a cycle here: A<B, B<C, C<A. Whatever order the three arrive in, the
// claim must settle on the same commit.
//
// Sabotage: restore the pairwise "ancestor, else hash" tie-break, and the
// final claim depends on the arrival order.
func TestBlobEdges_SameSecondIntroductionsAreATotalOrder(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	require.NoError(t, svc.CreateBranch(ctx, "side", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "side"))

	// Candidates for C first: each a real introduction of F on side.
	var cands []string
	for i := 0; i < 48; i++ {
		write(t, svc, "side", "kb/s.md", fmt.Sprintf("S %d", i))
		c, _ := write(t, svc, "side", "kb/f.md", "F", "kb/r.md")
		cands = append(cands, c)
		write(t, svc, "side", "kb/f.md", fmt.Sprintf("F side %d", i), "kb/r.md")
	}
	var a, b, c, fBlob string
	for i := 0; i < 200 && c == ""; i++ {
		// Each round starts from other content, so A is a real change
		// (re-writing identical bytes makes no commit on the branch).
		write(t, svc, "main", "kb/f.md", fmt.Sprintf("F before %d", i), "kb/r.md")
		a, fBlob = write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
		write(t, svc, "main", "kb/f.md", fmt.Sprintf("F changed %d", i), "kb/r.md")
		b, _ = write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
		for _, x := range cands {
			if b < x && x < a {
				c = x
				break
			}
		}
	}
	require.True(t, b < c && c < a, "precondition: B < C < A by hash")
	rh := svc.si.RepoForTest()
	for _, h := range []string{a, b, c} {
		got, err := rh.ReadBlobHashAtCommit(ctx, "kb/f.md", h)
		require.NoError(t, err)
		require.Equal(t, fBlob, got, "precondition: %s holds F", h)
	}
	res, err := db.Exec(`UPDATE commit_log SET committed_at = 1000 WHERE commit_hash IN (?, ?, ?)`, a, b, c)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 3, n, "precondition: A, B and C are each one commit_log row (one path)")

	perms := [][3]string{{a, b, c}, {a, c, b}, {b, a, c}, {b, c, a}, {c, a, b}, {c, b, a}}
	finals := map[string]bool{}
	for _, p := range perms {
		resetRefEdgesForTest(t, db)
		for _, h := range p {
			require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", fBlob, h, []string{"kb/r.md"}))
		}
		finals[refsCommitOf(t, db, fBlob)] = true
	}
	require.Len(t, finals, 1, "one claim whatever the arrival order")
	for h := range finals {
		require.Equal(t, c, h, "C: its same-second key (no same-second ancestor, smaller hash than A) is the least")
	}
	for _, x := range []string{a, b, c} {
		for _, y := range []string{a, b, c} {
			for _, z := range []string{a, b, c} {
				xy, err := svc.si.CommitEarlierForTest(ctx, x, y)
				require.NoError(t, err)
				yz, err := svc.si.CommitEarlierForTest(ctx, y, z)
				require.NoError(t, err)
				xz, err := svc.si.CommitEarlierForTest(ctx, x, z)
				require.NoError(t, err)
				if xy && yz {
					require.True(t, xz, "transitive")
				}
			}
		}
	}
}

// m3 (review of #693): the claim and the edges are written in ONE
// transaction. A failure between them (standing in for a crash) leaves the
// blob unclaimed and edgeless, so the next arrival resolves it in full.
//
// Sabotage: write the claim outside applyBlobRefs's transaction, and the
// failed write leaves the blob claimed with no edges.
func TestBlobEdges_ClaimAndEdgesAreAtomic(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	c, fBlob := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	resetRefEdgesForTest(t, db)

	calls := 0
	svc.si.SetBeforeRefEdgesWriteHookForTest(func() error { calls++; return fmt.Errorf("crash") })
	err := svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", fBlob, c, []string{"kb/r.md"})
	svc.si.SetBeforeRefEdgesWriteHookForTest(nil)
	require.Error(t, err)
	require.Equal(t, 1, calls, "the window between the claim and the edges was reached")
	require.Empty(t, refsCommitOf(t, db, fBlob), "no claim without its edges")
	require.Empty(t, allRefEdges(t, db))

	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", fBlob, c, []string{"kb/r.md"}))
	require.Equal(t, c, refsCommitOf(t, db, fBlob))
	require.Equal(t, 1, countEdgesOfBlob(allRefEdges(t, db), fBlob))
}

// Probe C of the review: a conflict-resolving merge followed by more commits;
// rebuilding every branch from scratch gives the incremental edges.
func TestBlobEdges_ConflictMergeRebuildEqualsIncremental(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.CreateBranch(ctx, "feature", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
	write(t, svc, "feature", "kb/f.md", "F feature", "kb/r.md")
	write(t, svc, "main", "kb/f.md", "F main", "kb/r.md")
	write(t, svc, "main", "kb/r.md", "R2")
	merged := []byte(testFactBody("F merged", 0.8, []string{"kb/r.md"}))
	_, err := svc.Merge().MergeIntoBranchLockedOpts(ctx, "feature", "main", merge.StrategyRefuse,
		map[string]merge.Resolution{"kb/f.md": {Body: merged}}, merge.MergeOpts{})
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Sync(ctx, "main"))
	write(t, svc, "main", "kb/z.md", "Z")
	incremental := allRefEdges(t, db)
	require.Len(t, incremental, 4, "F, F feature, F main, F merged: one edge each")
	resetRefEdgesForTest(t, db)
	for _, b := range []string{"main", "feature"} {
		require.NoError(t, svc.IndexManager().Rebuild(ctx, b, nil))
	}
	require.Equal(t, incremental, allRefEdges(t, db))
}

// Probe D of the review: a blob written on a side branch (or an experiment),
// carried into the read branch by a merge after R moved there; rebuilding the
// read branch alone gives the incremental edges, pinned at the side commit.
func TestBlobEdges_MergedSideBlobRebuildEqualsIncremental(t *testing.T) {
	for _, viaExperiment := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "experiment"}[viaExperiment], func(t *testing.T) {
			var svc *Service
			main := "main"
			if viaExperiment {
				svc, _ = sideStore(t)
				main = "agent/test"
			} else {
				svc = newMergeTestStore(t)
			}
			ctx := context.Background()
			db := svc.si.RepoForTest().DB()
			_, rBlob := write(t, svc, main, "kb/r.md", "R")
			side := "feature"
			if viaExperiment {
				_, err := svc.Merge().OpenExperiment(ctx, "x", "", main)
				require.NoError(t, err)
				side = "exp/x"
			} else {
				require.NoError(t, svc.CreateBranch(ctx, "feature", main))
				require.NoError(t, svc.IndexManager().Sync(ctx, "feature"))
			}
			cF, fBlob := write(t, svc, side, "kb/f.md", "F", "kb/r.md")
			write(t, svc, main, "kb/r.md", "R2")
			if viaExperiment {
				cr, err := svc.Merge().CommitExperiment(ctx, "x", nil)
				require.NoError(t, err)
				require.Equal(t, merge.ModeMerge, cr.Mode)
			} else {
				require.NoError(t, svc.Merge().MergeBranch(ctx, "feature", main, merge.StrategyLocalWins))
			}
			require.NoError(t, svc.IndexManager().Sync(ctx, main))
			incremental := allRefEdges(t, db)
			require.Equal(t, map[string]string{"kb/r.md": rBlob}, edgesOfBlob(incremental, fBlob))
			require.Equal(t, cF, refsCommitOf(t, db, fBlob))
			resetRefEdgesForTest(t, db)
			require.NoError(t, svc.IndexManager().Rebuild(ctx, main, nil))
			require.Equal(t, incremental, allRefEdges(t, db), "rebuild of the read branch only == incremental")
		})
	}
}

// B3 (round-2 review of #693; probe H): F is written in an experiment, so its
// blob is claimed at the experiment commit cE; the experiment is ROLLED BACK,
// which leaves that claim at a commit no branch reaches. The same bytes are
// then written on the agent branch, at the same path or a new one. The void
// claim must not stand: the blob is re-claimed at the agent commit and its old
// edges go with the old claim, so R's incoming on the agent branch lists the
// citer (as dev does) and rebuild == incremental.
//
// Sabotage: treat a void claim as live (blobClaim.live ignores void), and the
// claim stays at cE: incoming is empty and rebuild != incremental.
func TestBlobEdges_RolledBackExperimentClaimIsVoid(t *testing.T) {
	for _, samePath := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-path", false: "new-path"}[samePath], func(t *testing.T) {
			svc, _ := sideStore(t)
			ctx := context.Background()
			db := svc.si.RepoForTest().DB()
			cR, rBlob := write(t, svc, "agent/test", "kb/r.md", "R")
			_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
			require.NoError(t, err)
			cE, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
			require.NoError(t, svc.Merge().RollbackExperiment(ctx, "x"))
			require.Equal(t, cE, refsCommitOf(t, db, fBlob), "precondition: the claim survives the rollback")
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM branch_commits WHERE commit_hash = ?`, cE).Scan(&n))
			require.Zero(t, n, "precondition: no branch reaches cE")

			write(t, svc, "agent/test", "kb/z.md", "Z")
			p := "kb/f.md"
			if !samePath {
				p = "kb/g.md"
			}
			cA, b := write(t, svc, "agent/test", p, "F", "kb/r.md")
			require.Equal(t, fBlob, b, "precondition: the same blob")

			inc := allRefEdges(t, db)
			require.Equal(t, cA, refsCommitOf(t, db, fBlob), "the void claim is replaced at the agent commit")
			require.Equal(t, 1, countEdgesOfBlob(inc, fBlob), "one edge set: the void claim's edges are gone")
			for _, e := range inc {
				if e.SrcBlob == fBlob {
					require.Equal(t, p, e.SrcPath)
					require.Equal(t, cA, e.SourceCommit)
					require.Equal(t, rBlob, e.TgtBlob)
				}
			}
			in, err := svc.gq.IncomingAtCommit(ctx, "agent/test", "kb/r.md", cR)
			require.NoError(t, err)
			require.Len(t, in, 1, "R's incoming lists the citer")
			require.Equal(t, p, in[0].Path)
			require.False(t, in[0].Deleted)

			resetRefEdgesForTest(t, db)
			require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
			require.Equal(t, inc, allRefEdges(t, db), "rebuild == incremental")
		})
	}
}

// m6 (round-2 review): an arrival at a commit no branch reaches (a drain
// walking an experiment that is rolled back under it) does not claim there.
//
// Sabotage: let an unreachable passed commit compete like a reachable one
// (passedReachable := passedHolds), and the blob is claimed at cE.
func TestBlobEdges_UnreachablePassedCommitDoesNotClaim(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	cE, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	require.NoError(t, svc.Merge().RollbackExperiment(ctx, "x"))
	resetRefEdgesForTest(t, db)
	// The drain's call for the gone branch, at its (now orphaned) event.
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "exp/x", "kb/f.md", fBlob, cE, []string{"kb/r.md"}))
	require.Empty(t, refsCommitOf(t, db, fBlob), "no claim at a commit no branch reaches")
	require.Empty(t, allRefEdges(t, db))
}

// m4 (round-2 review; probe G): a CLAIMED blob arriving at a commit that
// holds it but is not its earliest introduction at that path. f: c1 = B,
// c2 = B', c3 = B; g on another branch: c4 = B, claimed first. The arrival
// at c3 must move the claim to c1 (f's earliest introduction), as a rebuild
// does.
//
// Sabotage: return the passed commit for a claimed blob without consulting
// earliestIntroduction, and the claim lands on c3 (rebuild: c1).
func TestBlobEdges_ClaimedBlobTakesItsEarliestIntroduction(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	c1, b := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	write(t, svc, "main", "kb/f.md", "F prime", "kb/r.md")
	c3, b3 := write(t, svc, "main", "kb/f.md", "F", "kb/r.md")
	require.Equal(t, b, b3)
	require.NoError(t, svc.CreateBranch(ctx, "other", "main"))
	require.NoError(t, svc.IndexManager().Sync(ctx, "other"))
	c4, b4 := write(t, svc, "other", "kb/g.md", "F", "kb/r.md")
	require.Equal(t, b, b4)
	resetRefEdgesForTest(t, db)
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "other", "kb/g.md", b, c4, []string{"kb/r.md"}))
	require.Equal(t, c4, refsCommitOf(t, db, b), "precondition: claimed via g at c4")
	require.NoError(t, svc.si.GraphAddDerivedFromAtCommitTxForTest(ctx, db, "main", "kb/f.md", b, c3, []string{"kb/r.md"}))
	require.Equal(t, c1, refsCommitOf(t, db, b), "the arrival at c3 resolves to f's earliest introduction c1")
	ofB := func() []refEdge {
		var out []refEdge
		for _, e := range allRefEdges(t, db) {
			if e.SrcBlob == b {
				out = append(out, e)
			}
		}
		return out
	}
	inc := ofB()
	require.Len(t, inc, 1)
	require.Equal(t, c1, inc[0].SourceCommit)
	resetRefEdgesForTest(t, db)
	for _, br := range []string{"main", "other"} {
		require.NoError(t, svc.IndexManager().Rebuild(ctx, br, nil))
	}
	require.Equal(t, inc, ofB(), "rebuild == incremental for B")
}

// m5 (round-2 review): an edge write that FAILS while the v8 upgrade drains a
// pending branch leaves that branch's mark, so the next rebuild re-resolves
// it; it is not cleared as if done.
//
// Sabotage: clear the mark whatever reResolveBranchRefs reports, and exp/x is
// no longer pending although F has no edges.
func TestBlobEdges_FailedDrainWriteKeepsThePendingMark(t *testing.T) {
	svc, _ := sideStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, _ := write(t, svc, "agent/test", "kb/r.md", "R")
	_, err := svc.Merge().OpenExperiment(ctx, "x", "", "agent/test")
	require.NoError(t, err)
	cF, fBlob := write(t, svc, "exp/x", "kb/f.md", "F", "kb/r.md")
	_, err = db.Exec(`DELETE FROM meta WHERE key = 'derived_from_per_blob'`)
	require.NoError(t, err)

	calls := 0
	svc.si.SetBeforeRefEdgesWriteHookForTest(func() error { calls++; return fmt.Errorf("write failed") })
	err = svc.IndexManager().Rebuild(ctx, "agent/test", nil)
	svc.si.SetBeforeRefEdgesWriteHookForTest(nil)
	require.NoError(t, err, "the rebuild itself succeeds")
	require.Positive(t, calls, "an edge write was attempted and failed")
	require.Equal(t, []string{"derived_from_per_blob_pending:exp/x"}, pendingRefBranches(t, db), "only the failed branch stays pending")
	require.Empty(t, edgesOfBlob(allRefEdges(t, db), fBlob))

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "agent/test", nil))
	require.Empty(t, pendingRefBranches(t, db))
	require.Equal(t, map[string]string{"kb/r.md": cR}, outPaths(t, svc, "exp/x", "kb/f.md", cF))
}

// branchFactCommit is branch_facts.commit_hash for (branch, path).
func branchFactCommit(t *testing.T, db *sql.DB, branch, path string) string {
	t.Helper()
	var c string
	require.NoError(t, db.QueryRow(`
		SELECT bf.commit_hash FROM branch_facts bf JOIN branches b ON b.id = bf.branch_id
		 WHERE b.name = ? AND bf.path = ?`, branch, path).Scan(&c))
	return c
}

// P1 (round-3 review of #693; the user's merge-case test): X (citing R) is
// written on branch A; B moves on (R changes, Z is written); A is merged into
// B with X carried unchanged, and B is synced. On B: X is in B's facts, X
// keeps exactly its one edge pinned at A's introducing commit, R's incoming
// lists X, and outgoing AND explain (HEAD Connections) for X are identical on
// B and on A. Run incrementally, and as the v8 upgrade of a store an older
// index wrote (B's recorded commit for X set to B's last non-merge commit, the
// commit LastCommitForPath named, which does not hold X), rebuilding B first
// or A first (B is then drained, not rebuilt).
//
// Sabotage: record LastCommitForPath's commit again in indexFile, and explain
// on B is empty (incremental); drop repairRecordedCommits from the drain, and
// "upgrade A first" keeps the empty explain.
func TestBlobEdges_MergeCarriesBlobUnchanged(t *testing.T) {
	const A, B = "side-a", "dest-b"
	type world struct {
		svc                      *Service
		db                       *sql.DB
		cR, cX, cZ, xBlob, rBlob string
	}
	build := func(t *testing.T) world {
		svc := newMergeTestStore(t)
		ctx := context.Background()
		db := svc.si.RepoForTest().DB()
		cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
		for _, b := range []string{A, B} {
			require.NoError(t, svc.CreateBranch(ctx, b, "main"))
			require.NoError(t, svc.IndexManager().Sync(ctx, b))
		}
		cX, xBlob := write(t, svc, A, "kb/x.md", "X", "kb/r.md")
		write(t, svc, B, "kb/r.md", "R on B")
		cZ, _ := write(t, svc, B, "kb/z.md", "Z")
		require.NoError(t, svc.Merge().MergeBranch(ctx, A, B, merge.StrategyLocalWins))
		require.NoError(t, svc.IndexManager().Sync(ctx, B))
		return world{svc, db, cR, cX, cZ, xBlob, rBlob}
	}
	check := func(t *testing.T, w world, label string) {
		ctx := context.Background()
		var blob string
		require.NoError(t, w.db.QueryRow(`
			SELECT f.blob_hash FROM branch_facts bf
			  JOIN facts f ON f.id = bf.fact_id
			  JOIN branches b ON b.id = bf.branch_id
			 WHERE b.name = ? AND bf.path = ?`, B, "kb/x.md").Scan(&blob), label)
		require.Equal(t, w.xBlob, blob, "%s: X is in B's facts, unchanged", label)
		require.Equal(t, w.cX, branchFactCommit(t, w.db, B, "kb/x.md"), "%s: B records X's own commit, which holds X", label)
		var ofX []refEdge
		for _, e := range allRefEdges(t, w.db) {
			if e.SrcBlob == w.xBlob {
				ofX = append(ofX, e)
			}
		}
		require.Len(t, ofX, 1, "%s: X has exactly one edge (no new edge from the merge)", label)
		require.Equal(t, "kb/r.md", ofX[0].TgtPath, label)
		require.Equal(t, w.rBlob, ofX[0].TgtBlob, "%s: pinned to R as of X's commit, not B's R", label)
		require.Equal(t, w.cX, ofX[0].SourceCommit, "%s: source_commit = A's introducing commit", label)
		require.Equal(t, w.cR, ofX[0].TargetCommit, label)
		require.Equal(t, w.cX, refsCommitOf(t, w.db, w.xBlob), label)
		in, err := w.svc.gq.IncomingAtCommit(ctx, B, "kb/r.md", w.cR)
		require.NoError(t, err)
		var paths []string
		for _, r := range in {
			paths = append(paths, r.Path)
		}
		require.Contains(t, paths, "kb/x.md", "%s: R's incoming on B lists X", label)
		headA, err := w.svc.HeadCommit(ctx, A)
		require.NoError(t, err)
		headB, err := w.svc.HeadCommit(ctx, B)
		require.NoError(t, err)
		onA, err := w.svc.gq.OutgoingAtCommit(ctx, A, "kb/x.md", headA)
		require.NoError(t, err)
		onB, err := w.svc.gq.OutgoingAtCommit(ctx, B, "kb/x.md", headB)
		require.NoError(t, err)
		require.Len(t, onA, 1, label)
		require.Equal(t, onA, onB, "%s: outgoing for X on B == on A", label)
		exA, err := w.svc.gq.ExplainFact(ctx, A, "kb/x.md")
		require.NoError(t, err)
		exB, err := w.svc.gq.ExplainFact(ctx, B, "kb/x.md")
		require.NoError(t, err)
		require.Len(t, exA.Outgoing, 1, label)
		require.Equal(t, exA.Outgoing, exB.Outgoing, "%s: explain outgoing on B == on A", label)
		require.Equal(t, exA.Incoming, exB.Incoming, "%s: explain incoming on B == on A", label)
	}

	t.Run("incremental", func(t *testing.T) {
		check(t, build(t), "incremental")
	})
	for _, first := range []string{B, A} {
		t.Run("upgrade-"+first+"-first", func(t *testing.T) {
			w := build(t)
			inc := allRefEdges(t, w.db)
			// The store an older index wrote: B recorded LastCommitForPath's
			// commit for X, which does not hold it.
			_, err := w.svc.si.RepoForTest().ReadBlobHashAtCommit(context.Background(), "kb/x.md", w.cZ)
			require.Error(t, err, "precondition: cZ does not hold X")
			_, err = w.db.Exec(`UPDATE branch_facts SET commit_hash = ? WHERE path = 'kb/x.md'
				AND branch_id = (SELECT id FROM branches WHERE name = ?)`, w.cZ, B)
			require.NoError(t, err)
			ex, err := w.svc.gq.ExplainFact(context.Background(), B, "kb/x.md")
			require.NoError(t, err)
			require.Empty(t, ex.Outgoing, "precondition: the old index's empty explain on B")

			_, err = w.db.Exec(`DELETE FROM meta WHERE key = 'derived_from_per_blob'`)
			require.NoError(t, err)
			require.NoError(t, w.svc.IndexManager().Rebuild(context.Background(), first, nil))
			require.Empty(t, pendingRefBranches(t, w.db), "every branch drained")
			check(t, w, "upgrade "+first+" first")
			require.Equal(t, inc, allRefEdges(t, w.db), "rebuild == incremental (%s first)", first)
		})
	}
}

// The commit recorded for a path on a branch HOLDS the path's blob there, and
// the incremental index and a rebuild record the same one: after a merge that
// carries X unchanged (X's own commit), and after a revert to earlier bytes
// (the revert, the live introduction, not the first one).
//
// Sabotage: indexFile records LastCommitForPath's commit again, and X on b
// records b's last non-merge commit, which does not hold X; rebuildFacts
// records the latest commit_log row again, and the rebuild records the merge
// for X (rebuild != incremental).
func TestBlobEdges_RecordedCommitHoldsTheBlob(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	rh := svc.si.RepoForTest()
	write(t, svc, "main", "kb/r.md", "R")
	for _, b := range []string{"a", "b"} {
		require.NoError(t, svc.CreateBranch(ctx, b, "main"))
		require.NoError(t, svc.IndexManager().Sync(ctx, b))
	}
	cX, xBlob := write(t, svc, "a", "kb/x.md", "X", "kb/r.md")
	write(t, svc, "b", "kb/z.md", "Z")
	require.NoError(t, svc.Merge().MergeBranch(ctx, "a", "b", merge.StrategyLocalWins))
	require.NoError(t, svc.IndexManager().Sync(ctx, "b"))
	// A revert on b: y = Y, Y', Y; then an unrelated commit.
	write(t, svc, "b", "kb/y.md", "Y")
	write(t, svc, "b", "kb/y.md", "Y prime")
	cY3, yBlob := write(t, svc, "b", "kb/y.md", "Y")
	write(t, svc, "b", "kb/w.md", "W")

	holds := func(path, commit, blob string) {
		got, err := rh.ReadBlobHashAtCommit(ctx, path, commit)
		require.NoError(t, err, "%s at %s", path, commit)
		require.Equal(t, blob, got)
	}
	incX, incY := branchFactCommit(t, db, "b", "kb/x.md"), branchFactCommit(t, db, "b", "kb/y.md")
	holds("kb/x.md", incX, xBlob)
	holds("kb/y.md", incY, yBlob)
	require.Equal(t, cX, incX, "the merge carried X: X's own commit")
	require.Equal(t, cY3, incY, "the revert: the live introduction")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, "b", nil))
	require.Equal(t, incX, branchFactCommit(t, db, "b", "kb/x.md"), "rebuild == incremental")
	require.Equal(t, incY, branchFactCommit(t, db, "b", "kb/y.md"), "rebuild == incremental")
}

// graphCounts is (Fact nodes, DERIVED_FROM edges, refs_commit marks).
func graphCounts(t *testing.T, db *sql.DB) [3]int {
	t.Helper()
	var c [3]int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&c[0]))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM edges WHERE type = 'DERIVED_FROM'`).Scan(&c[1]))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM node_props_text WHERE key_id = (SELECT id FROM property_keys WHERE key = 'refs_commit')`).Scan(&c[2]))
	return c
}

// onBranch reports whether a branch_commits row of branch names commit.
func onBranch(t *testing.T, db *sql.DB, branch, commit string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM branch_commits
		WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND commit_hash = ?`, branch, commit).Scan(&n))
	return n > 0
}

// incomingPaths is R's incoming on branch at cR as path -> deleted.
func incomingPaths(t *testing.T, svc *Service, branch, path, commit string) map[string]bool {
	t.Helper()
	in, err := svc.gq.IncomingAtCommit(context.Background(), branch, path, commit)
	require.NoError(t, err)
	m := map[string]bool{}
	for _, r := range in {
		_, dup := m[r.Path]
		require.False(t, dup, "%s listed once", r.Path)
		m[r.Path] = r.Deleted
	}
	return m
}

// B4 (round-4 review of #693; user ruling, OPTION 1): the same bytes X
// (citing R) are written on brA (cA) and later on brC (cC); the branches
// never merge; same path or another path. Incremental, and rebuilt with
// either branch first.
//   - R's incoming lists X on brA AND on brC: an edge is visible on a branch
//     iff the citing blob is held there, not iff its source_commit is on it.
//   - X's refs are resolved once, at the earliest introduction cA: on brC its
//     outgoing is R as resolved on brA (R_A @ cR), though brC has since
//     changed R to R_C; and the explain "changed since cited" input (the
//     branch's latest commit of R, explain.go readNode) differs from that pin.
//
// Then: brC is merged into brA and brA into brC (a fast-forward). The merges
// add no node, edge or mark; the mark is still cA, on both branches; R's
// incoming and X's Connections are identical on both; with two paths both
// are listed. brA is then dropped: the mark stays valid (cA is on brC) and
// nothing on brC changes.
//
// Sabotage: restore the source_commit-on-branch filter in IncomingAtCommit,
// and R's incoming on brC is empty.
func TestBlobEdges_SameBytesOnTwoBranches(t *testing.T) {
	for _, samePath := range []bool{true, false} {
		for _, mode := range []string{"incremental", "rebuild-A-first", "rebuild-C-first"} {
			name := map[bool]string{true: "same-path", false: "other-path"}[samePath] + "/" + mode
			t.Run(name, func(t *testing.T) {
				svc := newMergeTestStore(t)
				ctx := context.Background()
				db := svc.si.RepoForTest().DB()
				cR, rBlob := write(t, svc, "main", "kb/r.md", "R")
				for _, b := range []string{"brA", "brC"} {
					require.NoError(t, svc.CreateBranch(ctx, b, "main"))
					require.NoError(t, svc.IndexManager().Sync(ctx, b))
				}
				pA, pC := "kb/x.md", "kb/x.md"
				if !samePath {
					pC = "kb/x-copy.md"
				}
				cA, xBlob := write(t, svc, "brA", pA, "X", "kb/r.md")
				cRC, _ := write(t, svc, "brC", "kb/r.md", "R on C")
				write(t, svc, "brC", "kb/z.md", "Z")
				cC, b2 := write(t, svc, "brC", pC, "X", "kb/r.md")
				require.Equal(t, xBlob, b2, "precondition: same bytes")
				require.False(t, onBranch(t, db, "brC", cA), "precondition: cA is not on brC")
				switch mode {
				case "rebuild-A-first":
					resetRefEdgesForTest(t, db)
					require.NoError(t, svc.IndexManager().Rebuild(ctx, "brA", nil))
					require.NoError(t, svc.IndexManager().Rebuild(ctx, "brC", nil))
				case "rebuild-C-first":
					resetRefEdgesForTest(t, db)
					require.NoError(t, svc.IndexManager().Rebuild(ctx, "brC", nil))
					require.NoError(t, svc.IndexManager().Rebuild(ctx, "brA", nil))
				}
				require.Equal(t, cA, refsCommitOf(t, db, xBlob), "resolved once, at the earliest introduction")

				require.Equal(t, map[string]bool{pA: false}, incomingPaths(t, svc, "brA", "kb/r.md", cR), "R's incoming on brA")
				require.Equal(t, map[string]bool{pC: false}, incomingPaths(t, svc, "brC", "kb/r.md", cR), "R's incoming on brC lists X")
				in, err := svc.gq.IncomingAtCommit(ctx, "brC", "kb/r.md", cR)
				require.NoError(t, err)
				require.True(t, onBranch(t, db, "brC", in[0].Commit), "the row's commit is one of brC")
				got, err := svc.si.RepoForTest().ReadBlobHashAtCommit(ctx, pC, in[0].Commit)
				require.NoError(t, err)
				require.Equal(t, xBlob, got, "and holds X at that path")
				_ = cC

				exC, err := svc.gq.ExplainFact(ctx, "brC", pC)
				require.NoError(t, err)
				require.Len(t, exC.Outgoing, 1)
				require.Equal(t, "kb/r.md", exC.Outgoing[0].Path)
				require.Equal(t, cR, exC.Outgoing[0].Commit, "X on brC shows R as resolved on brA")
				pinned, err := svc.si.RepoForTest().ReadBlobHashAtCommit(ctx, "kb/r.md", exC.Outgoing[0].Commit)
				require.NoError(t, err)
				require.Equal(t, rBlob, pinned)
				head, ok := svc.FactQuery().LastCommitForPath(ctx, "brC", "kb/r.md")
				require.True(t, ok)
				require.Equal(t, cRC, head)
				require.NotEqual(t, exC.Outgoing[0].Commit, head, "changed since cited: brC's R is not the version X cites")

				// THEN MERGE.
				before := graphCounts(t, db)
				require.NoError(t, svc.Merge().MergeBranch(ctx, "brC", "brA", merge.StrategyLocalWins))
				require.NoError(t, svc.IndexManager().Sync(ctx, "brA"))
				require.NoError(t, svc.Merge().MergeBranch(ctx, "brA", "brC", merge.StrategyLocalWins))
				require.NoError(t, svc.IndexManager().Sync(ctx, "brC"))
				require.Equal(t, before, graphCounts(t, db), "the merges add no node, edge or mark")
				require.Equal(t, cA, refsCommitOf(t, db, xBlob))
				require.True(t, onBranch(t, db, "brA", cA) && onBranch(t, db, "brC", cA), "the mark is valid on both")
				want := map[string]bool{pA: false, pC: false}
				inA, inC := incomingPaths(t, svc, "brA", "kb/r.md", cR), incomingPaths(t, svc, "brC", "kb/r.md", cR)
				require.Equal(t, want, inA, "both paths listed")
				require.Equal(t, inA, inC, "R's incoming identical on both branches")
				for _, p := range []string{pA, pC} {
					xa, err := svc.gq.ExplainFact(ctx, "brA", p)
					require.NoError(t, err)
					xc, err := svc.gq.ExplainFact(ctx, "brC", p)
					require.NoError(t, err)
					require.Len(t, xa.Outgoing, 1, p)
					require.Equal(t, xa, xc, "%s: Connections identical on both branches", p)
				}

				exBefore, err := svc.gq.ExplainFact(ctx, "brC", pC)
				require.NoError(t, err)
				inBefore, err := svc.gq.IncomingAtCommit(ctx, "brC", "kb/r.md", cR)
				require.NoError(t, err)
				require.NoError(t, svc.rh.DropBranch(ctx, "brA"))
				require.Equal(t, cA, refsCommitOf(t, db, xBlob))
				require.True(t, onBranch(t, db, "brC", cA), "the mark stays valid: cA is on brC")
				require.Equal(t, before, graphCounts(t, db))
				exAfter, err := svc.gq.ExplainFact(ctx, "brC", pC)
				require.NoError(t, err)
				inAfter, err := svc.gq.IncomingAtCommit(ctx, "brC", "kb/r.md", cR)
				require.NoError(t, err)
				require.Equal(t, exBefore, exAfter, "nothing on brC changes")
				require.Equal(t, inBefore, inAfter)
			})
		}
	}
}

// OPTION 1 when the resolved target's commit is NOT on the reading branch: R
// changes on brA before X is written there, so X pins R_A at cRA, a brA-only
// commit; the same bytes X are written on brC. On brC, X's Connections show
// R @ cRA (no dropped row), the commit-anchored gate the REST read of that
// target uses (FactLiveAtCommit) passes, the version reads back as R_A, and
// R's incoming on brC at cRA lists X. brC's R (never R_A) differs from the
// pin, which is what explain's "changed since cited" compares.
func TestBlobEdges_TargetResolvedOnAnotherBranchRendersOnTheReader(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	write(t, svc, "main", "kb/r.md", "R")
	for _, b := range []string{"brA", "brC"} {
		require.NoError(t, svc.CreateBranch(ctx, b, "main"))
		require.NoError(t, svc.IndexManager().Sync(ctx, b))
	}
	cRA, rABlob := write(t, svc, "brA", "kb/r.md", "R on A")
	cA, xBlob := write(t, svc, "brA", "kb/x.md", "X", "kb/r.md")
	// Commits on brC first, so cC is later than cA even inside one second
	// (the order counts same-second ancestors).
	for i := 0; i < 3; i++ {
		write(t, svc, "brC", fmt.Sprintf("kb/filler-%d.md", i), fmt.Sprintf("F%d", i))
	}
	cC, _ := write(t, svc, "brC", "kb/x.md", "X", "kb/r.md")
	require.Equal(t, cA, refsCommitOf(t, db, xBlob), "precondition: resolved at cA on brA")
	require.False(t, onBranch(t, db, "brC", cRA), "precondition: cRA is not on brC")

	out, err := svc.gq.OutgoingAtCommit(ctx, "brC", "kb/x.md", cC)
	require.NoError(t, err)
	require.Len(t, out, 1, "no dropped row")
	require.Equal(t, cRA, out[0].Commit)
	ex, err := svc.gq.ExplainFact(ctx, "brC", "kb/x.md")
	require.NoError(t, err)
	require.Equal(t, out, ex.Outgoing)
	live, err := svc.FactQuery().FactLiveAtCommit(ctx, "brC", "kb/r.md", cRA)
	require.NoError(t, err)
	require.True(t, live, "the REST gate (factPresentAtCommitOr404) passes on brC: no 404")
	res, err := svc.Facts().ReadFact(ctx, "brC", "kb/r.md", &facts.ReadFactOpts{AtCommit: cRA})
	require.NoError(t, err)
	require.Contains(t, res.Content, "R on A", "the pinned version reads back on brC")
	pinned, err := svc.si.RepoForTest().ReadBlobHashAtCommit(ctx, "kb/r.md", cRA)
	require.NoError(t, err)
	require.Equal(t, rABlob, pinned)
	require.Equal(t, map[string]bool{"kb/x.md": false}, incomingPaths(t, svc, "brC", "kb/r.md", cRA))
	head, ok := svc.FactQuery().LastCommitForPath(ctx, "brC", "kb/r.md")
	require.True(t, ok)
	require.NotEqual(t, cRA, head, "changed since cited on brC")
}

// A citer the reading branch held and then retracted is still listed there,
// as retracted (kb/decisions/store/temporal/retracted-fact-semantics), even
// when the citing blob's edges were resolved on another branch: the same bytes
// X are written on brA, then on brC at another path, then retracted on brC.
// R's incoming on brC lists kb/x-copy.md with deleted=true, at a commit of
// brC that held it.
//
// Sabotage: count only the branch's current holdings (drop the path_changes
// half of blobHoldings), and the retracted citer disappears from brC.
func TestBlobEdges_RetractedCiterOnTheSecondBranchIsListed(t *testing.T) {
	svc := newMergeTestStore(t)
	ctx := context.Background()
	db := svc.si.RepoForTest().DB()
	cR, _ := write(t, svc, "main", "kb/r.md", "R")
	for _, b := range []string{"brA", "brC"} {
		require.NoError(t, svc.CreateBranch(ctx, b, "main"))
		require.NoError(t, svc.IndexManager().Sync(ctx, b))
	}
	write(t, svc, "brA", "kb/x.md", "X", "kb/r.md")
	write(t, svc, "brC", "kb/z.md", "Z")
	cC, _ := write(t, svc, "brC", "kb/x-copy.md", "X", "kb/r.md")
	_, err := svc.Facts().DeleteFact(ctx, "brC", "kb/x-copy.md", "retract")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"kb/x-copy.md": true}, incomingPaths(t, svc, "brC", "kb/r.md", cR))
	in, err := svc.gq.IncomingAtCommit(ctx, "brC", "kb/r.md", cR)
	require.NoError(t, err)
	require.Equal(t, cC, in[0].Commit, "the brC commit that held it")
	require.True(t, onBranch(t, db, "brC", in[0].Commit))
}
