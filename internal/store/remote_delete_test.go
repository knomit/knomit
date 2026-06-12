package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeleteRemote_RemovesRowGitRemoteAndIsIdempotent verifies that
// DeleteRemote clears both the remotes table row and the git remote config,
// and that calling it again on an already-removed remote is a no-op.
func TestDeleteRemote_RemovesRowGitRemoteAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	require.NoError(t, svc.Remote().SetRemote("origin", "https://example.com/repo.git", "main", "agent/test", 300, 300, "", ""))
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got, "precondition: origin must exist after SetRemote")

	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	_, ok := cfg.Remotes["origin"]
	require.True(t, ok, "precondition: git remote must be configured")

	require.NoError(t, svc.Remote().DeleteRemote("origin"))

	got, err = svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, got, "remotes row must be gone after DeleteRemote")

	cfg, err = svc.rh.repo.Config()
	require.NoError(t, err)
	_, ok = cfg.Remotes["origin"]
	require.False(t, ok, "git remote must be removed after DeleteRemote")

	// Idempotent: deleting again is a no-op, not an error.
	require.NoError(t, svc.Remote().DeleteRemote("origin"))
}
