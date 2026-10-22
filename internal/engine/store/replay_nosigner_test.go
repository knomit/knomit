package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"knomit/internal/engine/store/fsck"
	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/merge"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestReplayCommit_NoSignerLeavesNoObjects: the rewind path (rebase
// fallback) resolves the signer BEFORE replayCommit writes the synthetic base
// or any merged tree, so a signer-less replay is ErrNoSigner and leaves the
// object store exactly as it was (#317 review residual). This package's
// TestMain installs a fallback signer; the test removes it for its duration
// (no store test runs in parallel).
func TestReplayCommit_NoSignerLeavesNoObjects(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	writeMergeFact(t, svc, "agent/test", "kb/local.md", "L", "v1")
	disjoint := makeDisjointRoot(t, svc, "kb/rewind.md", "disjoint root")
	require.NoError(t, svc.rh.Storer().SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), disjoint)))
	before, err := svc.Integrity().Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	require.True(t, before.IsStrictlyClean(), "precondition: %v", before.Issues)

	vcs.SetTestFallbackSigner(nil)
	t.Cleanup(func() { vcs.SetTestFallbackSigner(fallbackTestSigner()) })
	svc.SetSigner(nil)

	_, err = svc.rr.ReconcileAgentForTest(context.Background(), "agent/test", "main", merge.StrategyLocalWins, true)
	require.True(t, errors.Is(err, model.ErrNoSigner), "a signer-less replay must be ErrNoSigner, got %v", err)
	after, err := svc.Integrity().Verify(context.Background(), fsck.VerifyOpts{})
	require.NoError(t, err)
	require.True(t, after.IsStrictlyClean(), "a refused replay must leave nothing behind: %v", after.Issues)
}
