package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RemoteHandle wraps a bare git repository on disk that one or more
// RepoHandles use as their origin. It provides methods that a true
// multi-agent test needs but the knomit Remote interface doesn't expose:
// direct writes to main, merging an agent branch into main, and deliberate
// object-store corruption for Verify tests.
//
// Uses real git via os/exec for ops the production code doesn't handle,
// matching the exact pattern in internal/store/repo_test.go:
//
//	exec.Command("git", "init", "--bare", remoteDir).Run()
type RemoteHandle struct {
	sb   *Storyboard
	name string
	dir  string // absolute path to the bare git directory
	url  string // file:// URL suitable for go-git
}

// BareRemote creates a new bare git remote at <storyboard-tempdir>/remotes/<name>
// and returns a handle to it. Call this once per test that needs remote-round-trip
// scenarios, then pass the returned RemoteHandle to RepoHandle.Connect (below).
//
// The bare repo starts empty. The first Push from any RepoHandle populates it.
func (sb *Storyboard) BareRemote(name string) *RemoteHandle {
	t := sb.t
	t.Helper()
	dir := filepath.Join(sb.homeDir, "remotes", name)
	mustGit(t, "", "init", "--bare", dir)
	return &RemoteHandle{
		sb:   sb,
		name: name,
		dir:  dir,
		url:  "file://" + dir,
	}
}

// URL returns the file:// URL of the bare remote. Used by Connect and by
// any test that wants to construct its own go-git Clone.
func (r *RemoteHandle) URL() string { return r.url }

// Dir returns the absolute path to the bare git directory on disk. Used by
// Corrupt and by any test that needs to manipulate the bare repo directly.
func (r *RemoteHandle) Dir() string { return r.dir }

// Name returns the Storyboard-assigned name for this remote (used for
// error messages and debugging).
func (r *RemoteHandle) Name() string { return r.name }

// mustGit runs `git <args>` in dir (or the process cwd if dir is ""),
// failing the test with the full stdout+stderr output on any non-zero
// exit. Used by BareRemote, MergeIntoMain, and WriteMain.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Disable gpg signing and use a stable identity so test commits are
	// deterministic. The bare repo doesn't care about identity but the
	// worktree clones used by MergeIntoMain / WriteMain do.
	cmd.Env = append([]string{
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}, envPassthrough()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed in %s:\n%v\n%s", args, dir, err, out)
	}
}

// envPassthrough returns the minimum env needed for `git` to run
// successfully (PATH, HOME, etc.) without inheriting developer config.
func envPassthrough() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
}
