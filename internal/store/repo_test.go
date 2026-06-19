package store

import (
	"errors"
	"os"
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

// TestCloneFrom_BareLocalPathAnonymous verifies that a populated local repo
// referenced by a BARE absolute path (no file:// scheme) clones successfully
// with nil (anonymous) auth — the local-origin + no-auth path.
func TestCloneFrom_BareLocalPathAnonymous(t *testing.T) {
	remoteDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = remoteDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(remoteDir, "README.md"), []byte("hi"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")

	svc, err := Open(filepath.Join(t.TempDir(), "clone.db"))
	require.NoError(t, err)
	defer svc.Close()

	// Bare absolute path, no scheme, nil auth.
	require.NoError(t, svc.CloneFrom(remoteDir, nil, nil))
}
