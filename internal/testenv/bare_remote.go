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

// MergeIntoMain merges the named branch into main on the bare remote.
// Simulates the planned merge-to-main feature: an agent's branch has
// been pushed to the remote, and now its content is being promoted into
// the consensus main branch (which knomit's MCP server never writes
// directly — it's a remote-side operation in the blueprint).
//
// Implemented via a clone-merge-push cycle: clone the bare repo into a
// scratch worktree, check out main, merge the named branch, push back
// to the bare repo. The bare repo never has a worktree, so the merge
// must happen in a transient clone.
//
// The branch must already exist on the bare repo (i.e. some agent
// previously pushed it). This is the helper Phase 3 Category E and the
// blueprint scenario rely on.
func (r *RemoteHandle) MergeIntoMain(branch, message string) {
	t := r.sb.t
	t.Helper()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	// Make sure main exists locally and is checked out. The clone
	// inherits whatever HEAD the bare repo had — usually main if any
	// agent has pushed main, otherwise the first pushed branch. Force
	// a checkout of main if the clone landed on something else.
	mustGit(t, work, "checkout", "-B", "main", "origin/main")

	// Merge the named branch with --no-ff so a real merge commit is
	// created (otherwise a fast-forward would just advance the ref and
	// not produce a distinct merge commit, which the test scenario
	// expects to be visible).
	mustGit(t, work, "merge", "--no-ff", "-m", message,
		"--allow-unrelated-histories", "origin/"+branch)

	mustGit(t, work, "push", "origin", "main")
}

// WriteMain writes a fact directly to main on the bare remote in a new
// commit. Simulates a "third-party agent's already-promoted change" —
// the test pretends another agent pushed and merged-to-main a fact, so
// when our agent under test syncs, it sees that fact arrive from origin/main.
//
// Implemented via the same clone-edit-push pattern as MergeIntoMain. If
// origin/main does not yet exist on the bare remote (i.e. nothing has
// been pushed to main yet), the helper bootstraps a fresh main with the
// fact as its root commit. This lets tests model the "third-party seeds
// main" path without requiring an unrelated upstream push first.
func (r *RemoteHandle) WriteMain(path string, spec FactSpec, message string) {
	t := r.sb.t
	t.Helper()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	// Check whether origin/main exists on the freshly-cloned remote.
	if hasRef(work, "refs/remotes/origin/main") {
		mustGit(t, work, "checkout", "-B", "main", "origin/main")
	} else {
		// No main yet — initialise an orphan branch so the first commit is
		// a fresh root.
		mustGit(t, work, "checkout", "--orphan", "main")
		// `clone` checked out HEAD; remove any inherited working-tree files
		// so the orphan branch starts clean.
		mustGit(t, work, "rm", "-rf", "--ignore-unmatch", ".")
	}

	full := filepath.Join(work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("WriteMain: mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(spec.Build()), 0o644); err != nil {
		t.Fatalf("WriteMain: write %s: %v", full, err)
	}
	mustGit(t, work, "add", path)
	mustGit(t, work, "commit", "-m", message)
	mustGit(t, work, "push", "origin", "main")
}

// DeleteMain removes a path from main on the bare remote in a new commit.
// Simulates a forward delete on the consensus branch — e.g. an admin
// scrubbing a file after some agent had pushed a chain that included it.
// Mirrors WriteMain's clone-edit-push pattern but does `git rm` + commit
// instead of writing a file. The path must already exist on origin/main;
// the helper fails the test otherwise.
func (r *RemoteHandle) DeleteMain(path, message string) {
	t := r.sb.t
	t.Helper()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	if !hasRef(work, "refs/remotes/origin/main") {
		t.Fatalf("DeleteMain: origin/main does not exist on %s", r.name)
	}
	mustGit(t, work, "checkout", "-B", "main", "origin/main")
	mustGit(t, work, "rm", path)
	mustGit(t, work, "commit", "-m", message)
	mustGit(t, work, "push", "origin", "main")
}

// WriteDisjointRootOnMain writes a brand-new root commit on main of the
// bare remote with no relation to any previous commit. Used by tests that
// model the "origin/main was force-rewound by an admin" recovery path
// (G6). The supplied path is created with the supplied content (raw
// string, not a FactSpec — this helper is for non-knomit-shaped content
// such as the admin's recovery seed).
func (r *RemoteHandle) WriteDisjointRootOnMain(path, content, msg string) {
	t := r.sb.t
	t.Helper()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)
	mustGit(t, work, "checkout", "--orphan", "fresh-main")
	// Remove anything inherited from the previous HEAD.
	mustGit(t, work, "rm", "-rf", "--ignore-unmatch", ".")

	full := filepath.Join(work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("WriteDisjointRootOnMain: mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteDisjointRootOnMain: write %s: %v", full, err)
	}
	mustGit(t, work, "add", path)
	mustGit(t, work, "commit", "-m", msg)
	// Force-push the orphan onto refs/heads/main, replacing whatever was
	// there before.
	mustGit(t, work, "push", "--force", "origin", "fresh-main:main")
}

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

// hasRef returns true when `git show-ref --verify <ref>` exits 0 in dir.
// Used by WriteMain to decide whether origin/main already exists on the
// cloned remote (and a normal checkout works) or whether the helper has
// to bootstrap an orphan main.
func hasRef(dir, ref string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", ref)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}, envPassthrough()...)
	return cmd.Run() == nil
}
