package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A read-only store refuses every AUTHORED commit on every branch, at the seam
// all three write doors share. This is the guarantee behind a subscription: a
// forgotten call site cannot leak a commit onto the followed branch.
func TestReadOnlyStore_RefusesAuthoredWritesOnEveryBranch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	svc.SetReadOnly(true)

	ctx := context.Background()
	for _, branch := range []string{"main", "agent/test"} {
		_, err := svc.Facts().WriteFact(ctx, branch, "kb/x.md", "---\ntype: observation\n---\n# x\n", "m", "created")
		require.ErrorIs(t, err, ErrRepoReadOnly, "WriteFact on %s", branch)
		_, err = svc.Facts().WriteRootFile(ctx, branch, "README.md", "hi", "m", "updated")
		require.ErrorIs(t, err, ErrRepoReadOnly, "WriteRootFile on %s", branch)
		_, err = svc.Facts().DeleteFact(ctx, branch, "kb/x.md", "m")
		require.ErrorIs(t, err, ErrRepoReadOnly, "DeleteFact on %s", branch)
		_, _, err = svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{"kb/y.md": "y"}, nil, "m", "created")
		require.ErrorIs(t, err, ErrRepoReadOnly, "BatchWriteFacts on %s", branch)
	}

	// Nothing was committed: main's tip is unchanged.
	before := mustHeadHash(t, svc, "main")
	svc.SetReadOnly(false)
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/x.md", "---\ntype: observation\n---\n# x\n", "m", "created")
	require.NoError(t, err, "clearing the flag re-enables writes")
	require.NotEqual(t, before, mustHeadHash(t, svc, "main"))
}

// The refusal happens BEFORE the branch lock is taken — part of the gate's
// stated contract, and the reason it cannot interfere with the
// notifyCommit-under-the-branch-lock chokepoint: a refused write never enters
// that region at all.
//
// Asserted by holding the branch lock and requiring the write to come back
// anyway. Were the gate placed after lockBranch, this would deadlock until the
// test's timeout instead of returning.
func TestReadOnlyStore_RefusesBeforeTakingTheBranchLock(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	svc.SetReadOnly(true)

	unlock := svc.rh.lockBranch("main")
	defer unlock()

	done := make(chan error, 1)
	go func() {
		_, werr := svc.Facts().WriteFact(context.Background(), "main",
			"kb/x.md", "---\ntype: observation\n---\n# x\n", "m", "created")
		done <- werr
	}()

	select {
	case werr := <-done:
		require.ErrorIs(t, werr, ErrRepoReadOnly)
	case <-time.After(2 * time.Second):
		t.Fatal("WriteFact blocked on the branch lock: the read-only gate is inside the locked region, not before it")
	}
}
