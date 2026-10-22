package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store/remotesync"
)

// A sync that WE cancelled established nothing about the remote, so it must not
// overwrite what the last completed sync established.
//
// The reported case: an event that restarts or exits the Sync stage (an origin
// attach or detach, a store swap, an unmount) cancels the reconcile loop's
// life, and the loop starts with an immediate tick, so a fetch is routinely in
// flight when that lands. Such a tick died with `context canceled`, and that
// message was written to the remotes table and broadcast — so an operation
// that fully succeeded showed a red "sync failed — Sync: fetch: Get ...:
// context canceled" on the repo screen underneath a green last-push line.
//
// The distinction is the CALLER's context, not the error's text: a per-fetch
// netTimeout firing is a real failure of a real attempt and still gets
// recorded. Only ctx.Err() != nil means "we stopped this ourselves".
func TestSync_CallerCancellationLeavesStatusAlone(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))

	svc.Remote().SetOrigin(&remotesync.Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote()
	// What the last completed sync established.
	require.NoError(t, ri.UpdateRemoteStatusForTest("origin", "ok", nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, syncErr := ri.Sync(ctx, "agent/test", nil)
	require.Error(t, syncErr, "a cancelled sync still reports the cancellation to its caller")

	rem, err := ri.GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastStatus)
	require.Equal(t, "ok", *rem.LastStatus, "the cancelled sync overwrote the last established status")
	require.Nil(t, rem.LastError)
}

// Push writes its own status on the same rule, and is cancelled by the same
// restart: exiting the Sync stage can cancel the loop between the Sync and the
// Push.
func TestPush_CallerCancellationLeavesStatusAlone(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))

	svc.Remote().SetOrigin(&remotesync.Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote()
	require.NoError(t, ri.UpdateRemotePushStatusForTest("origin", "ok", nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, pushErr := ri.Push(ctx, "agent/test", nil)
	require.Error(t, pushErr)

	rem, err := ri.GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastPushStatus)
	require.Equal(t, "ok", *rem.LastPushStatus, "the cancelled push overwrote the last established status")
	require.Nil(t, rem.LastPushError)
}

// A caller DEADLINE is not a cancellation: a remote that does not answer
// inside the time its caller gave it has really failed. That failure must be
// persisted — it may be the only record a user gets that their remote was
// unreachable.
//
// This is the line abandonedByCaller has to hold: "we stopped this ourselves"
// (the Sync stage's life cancelled) versus "the attempt ran out of time".
// Testing ctx.Err() != nil alone conflates them and loses the second.
func TestSync_CallerDeadlineIsRecorded(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))

	svc.Remote().SetOrigin(&remotesync.Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote()
	require.NoError(t, ri.UpdateRemoteStatusForTest("origin", "ok", nil))

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	_, syncErr := ri.Sync(ctx, "agent/test", nil)
	require.Error(t, syncErr)

	rem, err := ri.GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastStatus)
	require.Equal(t, "error", *rem.LastStatus, "a remote that ran out of time still failed, and the user must be told")
}

func TestPush_EmptyBranchIsRefused(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))

	_, err = svc.ri.Push(context.Background(), "", nil)
	require.ErrorIs(t, err, remotesync.ErrNoAgentBranch)
}
