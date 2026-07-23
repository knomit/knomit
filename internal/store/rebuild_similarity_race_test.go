package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// similarToOutDegree counts a fact version's outgoing SIMILAR_TO edges.
func similarToOutDegree(t *testing.T, svc *Service, ctx context.Context, path, blobHash string) int {
	t.Helper()
	var n int
	require.NoError(t, svc.rh.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM edges e
		 WHERE e.type = ?
		   AND e.source_id = (
		     SELECT nl.node_id FROM node_labels nl
		      JOIN node_props_text p  ON p.node_id = nl.node_id
		      JOIN property_keys  kp ON kp.id = p.key_id AND kp.key = 'path'
		      JOIN node_props_text b  ON b.node_id = nl.node_id
		      JOIN property_keys  kb ON kb.id = b.key_id AND kb.key = 'blob_hash'
		      WHERE nl.label = ? AND p.value = ? AND b.value = ?)`,
		EdgeSimilarTo, NodeFact, path, blobHash).Scan(&n))
	return n
}

// TestRebuild_DoesNotDestroyConcurrentCrossBranchSimilarityEdges pins the
// hazard created by making Rebuild REPLACE the similarity layer.
//
// The graph is shared across branches but lockBranch is per-branch, so a writer
// on another branch runs fully concurrently with a Rebuild. Rebuild computes
// its edge list in the KNN pass, BEFORE its transaction opens. A blanket
// `DELETE FROM edges WHERE type='SIMILAR_TO'` inside that transaction therefore
// destroys edges the concurrent writer committed in between, and the re-merge
// cannot restore them — they are not in the snapshot. Nothing regenerates them
// either: that branch's Sync sees last_commit == HEAD and no-ops forever.
//
// The rendezvous is a hook, not a sleep: the writer runs from inside
// beforeSimTx, so its commit is guaranteed to land in the exact window between
// the KNN pass and the prune. With a sleep this test would pass vacuously on a
// fast machine by simply not overlapping.
func TestRebuild_DoesNotDestroyConcurrentCrossBranchSimilarityEdges(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()

	// Seed main so the rebuild has real work and a populated similarity layer.
	for _, p := range []string{"kb/a.md", "kb/b.md", "kb/c.md"} {
		_, err = svc.Facts().WriteFact(ctx, "main", p,
			testFactBody("shared subject matter for knn", 0.9, nil), "init "+p, "")
		require.NoError(t, err)
	}
	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))

	// Precondition: the layer is non-empty, so the assertions below cannot pass
	// vacuously on an empty graph.
	var baseline int
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeSimilarTo).Scan(&baseline))
	require.Greater(t, baseline, 0, "similarity layer must be populated for this test to mean anything")

	// A second branch, sharing the graph, not the branch lock.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))

	const racePath = "kb/raced.md"
	var racedBlob string

	// The writer fires once, from inside the window.
	fired := false
	svc.si.beforeSimTx = func() {
		if fired {
			return
		}
		fired = true
		_, werr := svc.Facts().WriteFact(ctx, "feature", racePath,
			testFactBody("shared subject matter for knn raced", 0.9, nil), "raced write", "")
		require.NoError(t, werr)
		require.NoError(t, svc.rh.db.QueryRowContext(ctx,
			`SELECT blob_hash FROM facts WHERE path = ?`, racePath).Scan(&racedBlob))
	}
	defer func() { svc.si.beforeSimTx = nil }()

	// Rebuild main. Its snapshot predates the raced write entirely.
	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))
	require.True(t, fired, "the hook must have run, else nothing was raced")
	require.NotEmpty(t, racedBlob)

	// The raced fact is live, has a node and a vector — it must also keep its
	// similarity edges. Before the scoped prune this was 0.
	require.Greater(t, similarToOutDegree(t, svc, ctx, racePath, racedBlob), 0,
		"a fact written on another branch during Rebuild must keep its SIMILAR_TO edges")
}

// The prune must still remove edges belonging to superseded versions, which is
// the whole reason Rebuild deletes anything. Scoping the delete must not
// weaken that.
func TestRebuild_StillPrunesSupersededVersionEdges(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	for _, p := range []string{"kb/p.md", "kb/q.md"} {
		_, err = svc.Facts().WriteFact(ctx, "main", p,
			testFactBody("subject matter", 0.9, nil), "init "+p, "")
		require.NoError(t, err)
	}
	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))

	// An orphan version: a node with no `facts` row, carrying an outgoing edge.
	// This is the shape a superseded version leaves behind, and no per-source
	// prune driven by the current fact set would ever visit it.
	ghost, err := graphMergeNode(ctx, svc.rh.db, NodeFact,
		map[string]string{"path": "kb/p.md", "blob_hash": "supersededblob"})
	require.NoError(t, err)
	live, err := graphNodeIDByProps(ctx, svc.rh.db, NodeFact, map[string]string{
		"path": "kb/q.md", "blob_hash": mustBlob(t, svc, ctx, "kb/q.md")})
	require.NoError(t, err)
	require.NotZero(t, live)
	require.NoError(t, graphMergeEdge(ctx, svc.rh.db, ghost, live, EdgeSimilarTo))

	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))

	var left int
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ? AND source_id = ?`,
		EdgeSimilarTo, ghost).Scan(&left))
	require.Zero(t, left, "edges of a superseded version must still be pruned")
}

func mustBlob(t *testing.T, svc *Service, ctx context.Context, path string) string {
	t.Helper()
	var bh string
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT blob_hash FROM facts WHERE path = ?`, path).Scan(&bh))
	return bh
}
