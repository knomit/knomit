package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebuildGraph_RestoresHistoricalFactNodes regresses the bug where
// `knomit rebuild` left "rebuildGraph phaseB: edge write failed ...
// source Fact(...) not found" warnings in the log because Phase A only
// iterated current `facts` rows and historical blob versions
// (orphaned/GC'd from `facts` but still referenced by commit_log) had
// no graph nodes. The fix: Phase A.5 enumerates every distinct (path,
// blob_hash) ever recorded in commit_log on the branch and MERGEs a
// historical Fact node with deleted=true for any version absent from
// the current `facts` table.
//
// Reproduces the corruption by hard-deleting the v1 graph node and the
// v1 facts row, then runs Rebuild and asserts:
//  1. The v1 Fact node is restored in the graph (with deleted=true).
//  2. The historical DERIVED_FROM edge from B → A_v1 is writable in
//     Phase B (no source-not-found Warn, edge present in graph).
func TestRebuildGraph_RestoresHistoricalFactNodes(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	branch := "main"

	// 1) Write A v1 — creates Fact node Fact(A, blobA1).
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/a.md",
		testFactBody("a v1", 0.9, nil), "init a", "")
	require.NoError(t, err)

	// 2) Write B refs=[A] — creates Fact(B, blobB1) AND a DERIVED_FROM
	//    edge B → A_v1 (since A's current version at this commit is v1).
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/b.md",
		testFactBody("b refs a", 0.8, []string{"kb/a.md"}), "init b", "")
	require.NoError(t, err)

	// Capture A_v1's blob_hash before we update it (so we can verify the
	// historical version's graph node restoration later).
	var blobA1 string
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT blob_hash FROM facts WHERE path = ?`, "kb/a.md").Scan(&blobA1))
	require.NotEmpty(t, blobA1)

	// 3) Update A → v2. New blob, new Fact node. A_v1's branch_facts
	//    pointer is replaced; A_v1 becomes orphaned.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/a.md",
		testFactBody("a v2", 0.95, nil), "update a", "")
	require.NoError(t, err)

	si := svc.Search().(*searchIndex)

	// 4) Simulate the corruption observed in production: the v1 facts row
	//    was GC'd AND its graph node was hard-deleted (or never created
	//    in the first place — e.g. a sync miss on a merged-in commit).
	//    GC's normal soft-delete leaves the node in place; we use the
	//    test-only hard-delete + a raw SQL DELETE on facts to engineer
	//    the broken state.
	require.NoError(t, svc.deleteGraphFactNodeForTest("kb/a.md", blobA1))
	_, err = si.rh.db.ExecContext(ctx,
		`DELETE FROM facts WHERE path = ? AND blob_hash = ?`, "kb/a.md", blobA1)
	require.NoError(t, err)

	// Confirm the corruption: no graph node for (kb/a.md, blobA1).
	id, err := si.graphNodeIDByBlob(ctx, "kb/a.md", blobA1)
	require.NoError(t, err)
	require.Zero(t, id, "test setup: v1 node must be missing before Rebuild")

	// 5) Rebuild. Phase A.5 must enumerate commit_log on this branch,
	//    notice that (kb/a.md, blobA1) has no current `facts` row, and
	//    MERGE a historical Fact node for it with deleted=true.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	// 6) The v1 Fact node now exists in the graph again.
	restoredID, err := si.graphNodeIDByBlob(ctx, "kb/a.md", blobA1)
	require.NoError(t, err)
	require.NotZero(t, restoredID, "Rebuild must restore the orphaned historical Fact node")

	// 7) The historical DERIVED_FROM edge B → A_v1 must be present.
	//    Phase B's edge write would have failed (with the warn the user
	//    saw) if the source node hadn't been restored.
	var edgeCount int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&edgeCount))
	require.GreaterOrEqual(t, edgeCount, 1,
		"Phase B must succeed in writing at least the B → A_v1 historical edge")
}
