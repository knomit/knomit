package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"knomit/test/testenv/gitserver"
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
	sb             *Storyboard
	name           string
	dir            string // absolute path to the bare git directory
	url            string // file:// or http:// URL the product clones/pushes
	upstreamBranch string // consensus branch on this remote (default "main")
	httpSrv        *gitserver.Server // non-nil when served over smart-HTTP (see BareRemoteHTTP)
}

// BareRemote creates a new bare git remote at <storyboard-tempdir>/remotes/<name>
// and returns a handle to it. Call this once per test that needs remote-round-trip
// scenarios, then pass the returned RemoteHandle to RepoHandle.Connect (below).
//
// The bare repo starts empty with "main" as its symbolic HEAD. The first
// Push from any RepoHandle populates it. For tests that need a non-"main"
// upstream (e.g. master), call BareRemoteWithBranch instead.
func (sb *Storyboard) BareRemote(name string) *RemoteHandle {
	return sb.BareRemoteWithBranch(name, "main")
}

// BareRemoteWithBranch is BareRemote parameterized on the upstream branch
// name. The bare repo's symbolic HEAD is set to refs/heads/<branch> so
// detectRemoteUpstream picks up the right default when a repo connects
// without explicitly choosing a branch.
func (sb *Storyboard) BareRemoteWithBranch(name, upstreamBranch string) *RemoteHandle {
	t := sb.t
	t.Helper()
	if upstreamBranch == "" {
		upstreamBranch = "main"
	}
	dir := filepath.Join(sb.homeDir, "remotes", name)
	mustGit(t, "", "init", "--bare", "--initial-branch="+upstreamBranch, dir)
	return &RemoteHandle{
		sb:             sb,
		name:           name,
		dir:            dir,
		url:            "file://" + dir,
		upstreamBranch: upstreamBranch,
	}
}

// MirrorOf creates a second bare remote at
// <storyboard-tempdir>/remotes/<name> whose object store is a real `git
// clone --bare` of src — the same objects, the same refs, and therefore the
// same root commit, served from a different path (and so a different URL).
//
// This models a "mirror": two hosting locations for one knowledge base. It
// exists to build fixtures for the identity-uniqueness guard
// (Registry.RecordRepoID), which the cheap origin-URL preflight
// (Manager.ActiveRepoWithOrigin) cannot see — two different URLs sail
// through that check, and only a root-commit comparison after the clone
// catches the duplicate. Replaying src's commits instead of cloning them
// would produce different commit hashes and defeat the fixture's whole
// purpose, so this must be a real clone, not a replay — see bare_remote.go's
// package doc for why real git via os/exec is used for exactly this kind of
// op the production code doesn't otherwise perform.
//
// src must already hold the history the mirror should share (e.g. via
// WriteMain) — MirrorOf snapshots src's current state once, at call time,
// and never re-syncs afterward.
func (sb *Storyboard) MirrorOf(name string, src *RemoteHandle) *RemoteHandle {
	t := sb.t
	t.Helper()
	dir := filepath.Join(sb.homeDir, "remotes", name)
	mustGit(t, "", "clone", "--bare", src.Dir(), dir)
	return &RemoteHandle{
		sb:             sb,
		name:           name,
		dir:            dir,
		url:            "file://" + dir,
		upstreamBranch: src.UpstreamBranch(),
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

// UpstreamBranch returns the consensus branch this remote uses (typically
// "main", configurable via BareRemoteWithBranch). Tests pass this to
// Connect (and the origin PUT) so the production code uses the right upstream.
func (r *RemoteHandle) UpstreamBranch() string {
	if r.upstreamBranch == "" {
		return "main"
	}
	return r.upstreamBranch
}

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
// The named branch must already exist on the bare repo (i.e. some
// agent previously pushed it). If origin/main does NOT yet exist on
// the bare remote — the steady-state shape for the post-rework model
// where the first agent push targets only agent/<host> — the helper
// bootstraps main from the named branch instead of attempting a merge,
// then pushes main. This matches what the eventual merge-to-main
// feature must do on first promotion of an agent branch to consensus.
func (r *RemoteHandle) MergeIntoMain(branch, message string) {
	t := r.sb.t
	t.Helper()
	up := r.UpstreamBranch()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	if !hasRef(work, "refs/remotes/origin/"+up) {
		// First promotion: no consensus upstream yet. Bootstrap from the
		// agent branch directly. The eventual merge-to-main feature has
		// to handle this same shape — the first agent to push creates
		// origin/<agent> on an otherwise empty (or upstream-less) remote,
		// and promotion seeds the upstream from it.
		mustGit(t, work, "checkout", "-B", up, "origin/"+branch)
		mustGit(t, work, "push", "origin", up)
		return
	}

	// Steady-state path: origin/<upstream> exists. Check it out and merge
	// the named branch on top with --no-ff so a real merge commit is
	// created (otherwise a fast-forward would just advance the ref and not
	// produce a distinct merge commit, which the test scenario expects to
	// be visible).
	mustGit(t, work, "checkout", "-B", up, "origin/"+up)
	mustGit(t, work, "merge", "--no-ff", "-m", message,
		"--allow-unrelated-histories", "origin/"+branch)
	mustGit(t, work, "push", "origin", up)
}

// SquashMergeIntoMain squash-merges the named branch into main on the
// bare remote. Simulates the "remote PR merged with squash" workflow: the
// agent's chain of N commits becomes one new commit on main whose parents
// are [previous main tip] only — the agent's commits are NOT in main's
// ancestry. This is the worst case for the old rebase-based reconcile
// (which would replay all N commits as orphans on the next sync) and the
// best case for the merge-based reconcile (which fast-forwards in O(1)).
//
// Implemented via the same clone-merge-push pattern as MergeIntoMain but
// using `git merge --squash` + `git commit` to produce a single new commit
// on main with no link to the agent branch's chain.
//
// origin/main must already exist on the bare remote — squash-merge into a
// missing main is meaningless. If main doesn't exist yet, the test should
// call WriteMain first to seed it.
func (r *RemoteHandle) SquashMergeIntoMain(branch, message string) {
	t := r.sb.t
	t.Helper()
	up := r.UpstreamBranch()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	if !hasRef(work, "refs/remotes/origin/"+up) {
		t.Fatalf("SquashMergeIntoMain: origin/%s does not exist on %s; call WriteMain first to seed it", up, r.name)
	}

	mustGit(t, work, "checkout", "-B", up, "origin/"+up)
	// --squash stages the branch's net diff without recording it as a merge;
	// the follow-up `commit` produces a single new commit with no second
	// parent. This is the canonical "squash-merge a PR" shape.
	mustGit(t, work, "merge", "--squash", "--allow-unrelated-histories", "origin/"+branch)
	mustGit(t, work, "commit", "-m", message)
	mustGit(t, work, "push", "origin", up)
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
	up := r.UpstreamBranch()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	// Check whether origin/<upstream> exists on the freshly-cloned remote.
	if hasRef(work, "refs/remotes/origin/"+up) {
		mustGit(t, work, "checkout", "-B", up, "origin/"+up)
	} else {
		// No upstream yet — initialise an orphan branch so the first commit
		// is a fresh root.
		mustGit(t, work, "checkout", "--orphan", up)
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
	mustGit(t, work, "push", "origin", up)
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
	up := r.UpstreamBranch()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)

	if !hasRef(work, "refs/remotes/origin/"+up) {
		t.Fatalf("DeleteMain: origin/%s does not exist on %s", up, r.name)
	}
	mustGit(t, work, "checkout", "-B", up, "origin/"+up)
	mustGit(t, work, "rm", path)
	mustGit(t, work, "commit", "-m", message)
	mustGit(t, work, "push", "origin", up)
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
	up := r.UpstreamBranch()

	work := t.TempDir()
	mustGit(t, "", "clone", r.dir, work)
	mustGit(t, work, "checkout", "--orphan", "fresh-upstream")
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
	// Force-push the orphan onto refs/heads/<upstream>, replacing whatever
	// was there before.
	mustGit(t, work, "push", "--force", "origin", "fresh-upstream:"+up)
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
