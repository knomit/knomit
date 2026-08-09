package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeleteRemote_RemovesStatusRowGitRemoteAndIsIdempotent verifies that
// DeleteRemote clears BOTH halves of what the repo holds about a remote — the
// git remote config and the sync/push status row — and that calling it again on
// an already-removed remote is a no-op.
//
// Connection identity is not among them any more: control.db (repos.Origins)
// owns it and repos deletes it there. The status row is still dropped, because
// a repo later re-pointed at a different origin would otherwise inherit the
// previous one's last_sync/last_error and report them as its own.
func TestDeleteRemote_RemovesStatusRowGitRemoteAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// A configured origin, as control.db injects it, plus the git remote and a
	// status row the first (failed) sync would have left behind.
	svc.SetOrigin(&Origin{URL: "https://example.com/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.com/repo.git", "main", "agent/test"))
	require.NoError(t, svc.Remote().RecordSyncError("origin", "boom"))

	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got, "precondition: an injected origin must surface")
	require.NotNil(t, got.LastError, "precondition: a status row must exist")

	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	_, ok := cfg.Remotes["origin"]
	require.True(t, ok, "precondition: git remote must be configured")

	require.NoError(t, svc.Remote().DeleteRemote("origin"))

	var n int
	require.NoError(t, svc.rh.db.QueryRow(
		`SELECT COUNT(*) FROM remotes WHERE name = 'origin'`).Scan(&n))
	require.Zero(t, n, "the status row must be gone after DeleteRemote")

	// Observable through the API too: no stale status clings to a re-injected
	// origin.
	got, err = svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Nil(t, got.LastError, "stale sync status must not survive DeleteRemote")
	require.Nil(t, got.LastStatus, "stale sync status must not survive DeleteRemote")

	cfg, err = svc.rh.repo.Config()
	require.NoError(t, err)
	_, ok = cfg.Remotes["origin"]
	require.False(t, ok, "git remote must be removed after DeleteRemote")

	// Clearing the injected origin is control.db's half; together they leave the
	// repo with no origin at all.
	svc.SetOrigin(nil)
	got, err = svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, got)

	// Idempotent: deleting again is a no-op, not an error.
	require.NoError(t, svc.Remote().DeleteRemote("origin"))
}
