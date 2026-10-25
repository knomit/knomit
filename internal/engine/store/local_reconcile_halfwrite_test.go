package store

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/model"
)

// missingLocalUpstream opens a store in the shape EnsureLocalUpstream exists
// for: origin/main at the agent tip (which carries kb/x.md), and NO local
// main — no ref, no branches row, no cached branch id. It returns the tip
// origin/main points at and the hash main had before it was dropped (an
// ancestor of that tip).
func missingLocalUpstream(t *testing.T) (svc *Service, originTip, oldMain string) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	ctx := context.Background()
	require.NoError(t, svc.InitRepoWithUpstream(ctx, map[string]string{}, "main", "agent/a"))
	_, err = svc.Facts().WriteFact(ctx, "agent/a", "kb/x.md", testFactBody("x", 0.9, nil), "x", "")
	require.NoError(t, err)
	originTip, err = svc.HeadCommit(ctx, "agent/a")
	require.NoError(t, err)
	oldMain, err = svc.HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.NotEqual(t, oldMain, originTip)
	require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", "main"), plumbing.NewHash(originTip))))
	require.NoError(t, svc.rh.DropBranch(ctx, "main"))
	requireNoLocalMain(t, svc)
	return svc, originTip, oldMain
}

// requireNoLocalMain asserts main has neither a ref nor a branches row: the
// state a refused bootstrap must leave behind.
func requireNoLocalMain(t *testing.T, svc *Service) {
	t.Helper()
	_, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("main"))
	require.Error(t, err, "refs/heads/main must not exist")
	_, err = svc.rh.BranchID(context.Background(), "main")
	require.ErrorIs(t, err, model.ErrBranchNotFound, "main must have no branches row")
}

// requireLocalMainRecorded asserts main is complete at want: the ref, the
// branches row, want recorded in branch_commits, and branch_facts holding
// kb/x.md (NotifyCommit's index sync ran). Each is read with its own SQL, not
// through CommitRecorded, so the check does not share the code under test.
func requireLocalMainRecorded(t *testing.T, svc *Service, want string) {
	t.Helper()
	ctx := context.Background()
	ref, err := svc.rh.Storer().Reference(plumbing.NewBranchReferenceName("main"))
	require.NoError(t, err)
	require.Equal(t, want, ref.Hash().String(), "main points where it should")
	svc.rh.ForgetBranchIDForTest("main")
	id, err := svc.rh.BranchID(ctx, "main")
	require.NoError(t, err, "branches row")
	var commits, facts int
	require.NoError(t, svc.rh.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`, id, want).Scan(&commits))
	require.Equal(t, 1, commits, "tip recorded on main in branch_commits")
	require.NoError(t, svc.rh.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM branch_facts WHERE branch_id = ?`, id).Scan(&facts))
	require.Positive(t, facts, "main's index synced (branch_facts)")
}

// parkedOnBranchLock reports whether some goroutine is inside
// EnsureLocalUpstream and blocked in LockBranch.
func parkedOnBranchLock() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "EnsureLocalUpstream") && strings.Contains(g, "LockBranch") &&
			strings.Contains(g, "RWMutex).Lock") {
			return true
		}
	}
	return false
}

// #587: a caller cancelled while parked on the upstream's branch lock (the
// Sync life behind an index Rebuild, at Unmount) used to take the lock with a
// dead ctx, write refs/heads/main, and then fail in EnsureBranch, leaving a
// ref with no branches row, commit_log or notify. The test holds the lock,
// waits until the call is visibly parked on it, cancels, and only then
// releases: the call must refuse without writing anything, and a later call
// with a live ctx must bootstrap main completely.
func TestEnsureLocalUpstream_CancelledWhileParkedOnTheLockWritesNothing(t *testing.T) {
	svc, originTip, _ := missingLocalUpstream(t)

	unlock := svc.rh.LockBranch("main")
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		created bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		created, err := svc.Remote().EnsureLocalUpstream(ctx, "main")
		done <- result{created, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !parkedOnBranchLock() {
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("EnsureLocalUpstream never parked on the branch lock")
		}
		runtime.Gosched()
	}
	cancel()
	unlock()
	res := <-done

	require.True(t, errors.Is(res.err, context.Canceled), "got %v", res.err)
	require.False(t, res.created)
	requireNoLocalMain(t, svc)

	created, err := svc.Remote().EnsureLocalUpstream(context.Background(), "main")
	require.NoError(t, err)
	require.True(t, created)
	requireLocalMainRecorded(t, svc, originTip)
}

// #587: a half-state that already exists — refs/heads/main written, its SQL
// bookkeeping not — is completed by the next call instead of being taken for
// a finished bootstrap. The bookkeeping is done at the ref's own hash: the ref
// is never moved, even though origin/main points elsewhere.
func TestEnsureLocalUpstream_CompletesAHalfWrittenUpstream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		withRow bool // the branches row was written, commit_log was not
	}{
		{name: "ref without a branches row"},
		{name: "ref and row without commit_log", withRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, originTip, oldMain := missingLocalUpstream(t)
			ctx := context.Background()
			// Point the half-written ref at origin's tip, then — separately —
			// move origin/main back to the old main, so "completed at the ref's
			// own hash" and "re-bootstrapped from origin" give different answers.
			require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
				plumbing.NewBranchReferenceName("main"), plumbing.NewHash(originTip))))
			require.NoError(t, svc.rh.Storer().SetReference(plumbing.NewHashReference(
				plumbing.NewRemoteReferenceName("origin", "main"), plumbing.NewHash(oldMain))))
			if tc.withRow {
				_, err := svc.rh.EnsureBranch(ctx, "main", "refs/heads/main")
				require.NoError(t, err)
			}

			created, err := svc.Remote().EnsureLocalUpstream(ctx, "main")
			require.NoError(t, err)
			require.True(t, created, "the half-state was completed")
			requireLocalMainRecorded(t, svc, originTip)

			created, err = svc.Remote().EnsureLocalUpstream(ctx, "main")
			require.NoError(t, err)
			require.False(t, created, "a complete upstream is left alone")
		})
	}
}
