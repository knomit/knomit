package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRebuild_GraphSnapshotDoesNotResurrectAConcurrentCrossBranchDelete pins
// the cross-branch race in rebuildGraph.
//
// `facts` rows and graph Fact nodes are keyed by version and shared by every
// branch, while lockBranch is per branch, so a delete on another branch runs
// fully concurrently with a Rebuild. rebuildGraph used to read its `facts`
// snapshot before opening its write transaction. A delete landing in between
// removed the facts row and soft-deleted the node; the transaction then
// re-merged the node from the stale snapshot and flipped it live — a live
// Fact node with no facts row, which Verify reports.
//
// The rendezvous is the beforeGraphTx hook, not a sleep: the delete commits in
// the exact window between the git-read cache and BeginTx.
func TestRebuild_GraphSnapshotDoesNotResurrectAConcurrentCrossBranchDelete(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "main"))
	ctx := context.Background()

	_, err = svc.Facts().WriteFact(ctx, "main", "kb/m.md", testFactBody("on main", 0.9, nil), "main fact", "")
	require.NoError(t, err)
	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))

	// A second branch holding the ONLY version of a fact, so deleting it there
	// removes its facts row and soft-deletes its node.
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))
	const racePath = "kb/feature-only.md"
	_, err = svc.Facts().WriteFact(ctx, "feature", racePath, testFactBody("feature only", 0.9, nil), "feature fact", "")
	require.NoError(t, err)

	fired := false
	svc.si.beforeGraphTx = func() {
		if fired {
			return
		}
		fired = true
		_, derr := svc.Facts().DeleteFact(ctx, "feature", racePath, "delete on feature")
		require.NoError(t, derr)
	}
	defer func() { svc.si.beforeGraphTx = nil }()

	require.NoError(t, svc.si.Rebuild(ctx, "main", nil))
	require.True(t, fired, "the hook must have run, else nothing was raced")

	report, err := svc.Verify(ctx, VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "a cross-branch delete during Rebuild must not leave a live node without a facts row: %v", report.Issues)
}
