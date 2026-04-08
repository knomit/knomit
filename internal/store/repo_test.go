package store

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCloneFrom_EmptyRemoteReturnsErrEmptyRemote verifies that CloneFrom
// returns the typed sentinel ErrEmptyRemote when the remote repository
// exists but has no commits/branches yet. Callers (web layer) interpret this
// to refuse the connection: knomit's sync model requires at least a "main"
// branch to merge into the agent branch.
func TestCloneFrom_EmptyRemoteReturnsErrEmptyRemote(t *testing.T) {
	remoteDir := t.TempDir()
	require.NoError(t, exec.Command("git", "init", "--bare", remoteDir).Run())

	svc, err := Open(filepath.Join(t.TempDir(), "clone.db"))
	require.NoError(t, err)
	defer svc.Close()

	err = svc.CloneFrom("file://"+remoteDir, nil, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrEmptyRemote), "empty remote must return ErrEmptyRemote, got: %v", err)
}

// TestCloneFrom_ErrorOnBadURL verifies that CloneFrom returns a non-nil
// error (and not ErrEmptyRemote) for an unreachable URL.
func TestCloneFrom_ErrorOnBadURL(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "clone.db"))
	require.NoError(t, err)
	defer svc.Close()

	err = svc.CloneFrom("file:///nonexistent/repo.git", nil, nil)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrEmptyRemote))
}
