// Tests for the configurable-upstream-branch fix: configureRemote must build
// the fetch refspec from the caller's branch name (not a hardcoded "main"),
// reconcileMain must operate on it, and InitFromRemote must detect the remote
// symbolic HEAD when the caller did not pick a branch.
//
// The upstream branch itself is stored in control.db now, so the tests that
// pinned the store's own writers (SetRemote / SetUpstreamBranch) live with
// their new owners: internal/repos/origins_test.go for the record, and
// internal/web/handlers_origin_hal_test.go for the refspec-first ordering.
package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestSync_FetchHoldsConfigReadLock regresses the data race where a config
// rewrite (ConfigureRemote → configureRemote: DeleteRemote+CreateRemote under
// configMu.Lock, which every origin change runs) could rewrite the git remote
// out from under an in-flight reconcile fetch. Sync now takes configMu.RLock
// across the origin check + the fetch, so while a config rewrite holds the write lock the fetch must block
// rather than race a half-rewritten remote.
func TestSync_FetchHoldsConfigReadLock(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// A configured remote — the bogus URL is fine: the fetch fails, but only
	// AFTER acquiring the read lock, which is the behaviour under test.
	svc.SetOrigin(&Origin{URL: "https://example.invalid/repo.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/repo.git", "main", "agent/test"))

	ri := svc.Remote().(*remoteIndex)

	// Simulate configureRemote mid-rewrite by holding the write lock.
	ri.rh.configMu.Lock()

	done := make(chan struct{})
	go func() {
		_, _ = ri.Sync(context.Background(), "agent/test", nil) // must block on configMu.RLock
		close(done)
	}()

	select {
	case <-done:
		ri.rh.configMu.Unlock()
		t.Fatal("Sync reached the fetch while a config rewrite held configMu — the fetch is not guarded")
	case <-time.After(200 * time.Millisecond):
		// Good: Sync is parked waiting for the read lock.
	}

	ri.rh.configMu.Unlock()
	select {
	case <-done: // resumes; the bogus fetch then fails harmlessly
	case <-time.After(5 * time.Second):
		t.Fatal("Sync did not resume after the config write lock was released")
	}
}

// TestConfigureRemote_RefspecUsesConfiguredUpstream: when configureRemote is
// given upstreamMain="master", the git config's fetch refspec must reference
// master (otherwise fetch would silently miss origin/master).
func TestConfigureRemote_RefspecUsesConfiguredUpstream(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	require.NoError(t, svc.rh.configureRemote("https://example.com/repo.git", "master", "agent/test"))

	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	rc, ok := cfg.Remotes["origin"]
	require.True(t, ok, "origin remote must be configured")
	require.Len(t, rc.Fetch, 2, "must write two refspecs (upstream + agent)")

	got := make(map[string]bool, len(rc.Fetch))
	for _, rs := range rc.Fetch {
		got[string(rs)] = true
	}
	require.True(t, got["+refs/heads/master:refs/remotes/origin/master"],
		"upstream refspec must reference master, not main: %v", rc.Fetch)
	require.True(t, got["+refs/heads/agent/test:refs/remotes/origin/agent/test"],
		"agent refspec missing: %v", rc.Fetch)
}

// TestReconcileMain_UsesConfiguredUpstream: reconcileMain must look up
// origin/<upstream> and advance the local branch named <upstream>. With the
// fix, calling reconcileMain(ctx, "master") on a repo where origin/master has
// advanced must fast-forward local master.
func TestReconcileMain_UsesConfiguredUpstream(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	// Initialize the repo using master as the consensus branch.
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, "master", "agent/test"))

	// Move master back to parent so reconcile has work to do.
	newMasterCommit := writeMergeFact(t, svc, "master", "kb/m.md", "M", "v1")
	parent, err := svc.rh.repo.CommitObject(plumbing.NewHash(newMasterCommit))
	require.NoError(t, err)
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("master"), parent.ParentHashes[0]),
	))
	require.NoError(t, svc.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "master"), plumbing.NewHash(newMasterCommit)),
	))

	res, err := svc.rh.reconcileMain(context.Background(), "master")
	require.NoError(t, err)
	require.Equal(t, ModeFF, res.Mode)
	require.Equal(t, plumbing.NewHash(newMasterCommit), mustHeadHash(t, svc, "master"))
}

// TestInitFromRemote_DetectsRemoteHEAD: when the caller passes empty
// upstreamMain, InitFromRemote must discover the remote's symbolic HEAD and
// use that branch (here: master). Falls back to "main" only if detection
// fails.
func TestInitFromRemote_DetectsRemoteHEAD(t *testing.T) {
	// Build a bare remote whose HEAD points to master (not main).
	bareDir := t.TempDir()
	mustRun(t, "", "git", "init", "--bare", "--initial-branch=master", bareDir)

	// Seed a commit on master via a worktree clone.
	work := t.TempDir()
	mustRun(t, "", "git", "clone", bareDir, work)
	mustRun(t, work, "git", "checkout", "-B", "master")
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644))
	mustRun(t, work, "git", "config", "user.email", "t@t")
	mustRun(t, work, "git", "config", "user.name", "t")
	mustRun(t, work, "git", "add", "seed.txt")
	mustRun(t, work, "git", "commit", "-m", "seed master")
	mustRun(t, work, "git", "push", "origin", "master")
	mustRun(t, bareDir, "git", "symbolic-ref", "HEAD", "refs/heads/master")

	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	// Empty upstreamMain → detection must find "master", and must REPORT it:
	// the caller persists the returned name into control.db's origin.
	upstream, wasEmpty, err := svc.InitFromRemote("file://"+bareDir, nil, "", "agent/test", nil)
	require.NoError(t, err)
	require.Equal(t, "master", upstream, "InitFromRemote must return the branch it resolved")
	require.False(t, wasEmpty, "a remote with refs must be reported as the CLONE path, not the empty one")

	got, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("master"))
	require.NoError(t, err, "local master must exist after InitFromRemote detection")
	require.NotEqual(t, plumbing.ZeroHash, got.Hash())

	// Local "main" must NOT have been created.
	_, mainErr := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("main"))
	require.ErrorIs(t, mainErr, plumbing.ErrReferenceNotFound,
		"local main must not be created when upstream is master")
}

// TestInitFromRemote_PrefersMainOverAgentBranchHEAD regresses the clone bug:
// a remote whose symbolic HEAD points at an agent branch (e.g. its GitHub
// default branch was set to agent/<host>) must NOT make that agent branch the
// local consensus upstream. When the remote HAS "main", InitFromRemote must
// adopt "main" regardless of where HEAD points.
func TestInitFromRemote_PrefersMainOverAgentBranchHEAD(t *testing.T) {
	bareDir := t.TempDir()
	mustRun(t, "", "git", "init", "--bare", "--initial-branch=main", bareDir)

	work := t.TempDir()
	mustRun(t, "", "git", "clone", bareDir, work)
	mustRun(t, work, "git", "config", "user.email", "t@t")
	mustRun(t, work, "git", "config", "user.name", "t")

	// main: the consensus branch.
	mustRun(t, work, "git", "checkout", "-B", "main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644))
	mustRun(t, work, "git", "add", "seed.txt")
	mustRun(t, work, "git", "commit", "-m", "seed main")
	mustRun(t, work, "git", "push", "origin", "main")

	// An agent branch, and point the remote's HEAD at it (the misconfiguration).
	mustRun(t, work, "git", "checkout", "-B", "agent/other-host")
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644))
	mustRun(t, work, "git", "add", "a.txt")
	mustRun(t, work, "git", "commit", "-m", "agent work")
	mustRun(t, work, "git", "push", "origin", "agent/other-host")
	mustRun(t, bareDir, "git", "symbolic-ref", "HEAD", "refs/heads/agent/other-host")

	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	// Empty upstreamMain → must prefer "main", NOT the agent-branch HEAD.
	upstream, wasEmpty, err := svc.InitFromRemote("file://"+bareDir, nil, "", "agent/test", nil)
	require.NoError(t, err)
	require.Equal(t, "main", upstream, "InitFromRemote must return the branch it resolved")
	require.False(t, wasEmpty, "a remote with refs must be reported as the CLONE path, not the empty one")

	// Local "main" must have been created as the upstream.
	_, err = svc.rh.gits.Reference(plumbing.NewBranchReferenceName("main"))
	require.NoError(t, err, "InitFromRemote must adopt main as upstream when the remote has it")

	// The configured fetch refspec must reference main, not the agent branch.
	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	rc := cfg.Remotes["origin"]
	got := make(map[string]bool, len(rc.Fetch))
	for _, rs := range rc.Fetch {
		got[string(rs)] = true
	}
	require.True(t, got["+refs/heads/main:refs/remotes/origin/main"],
		"upstream refspec must reference main, not the agent-branch HEAD: %v", rc.Fetch)
	require.False(t, got["+refs/heads/agent/other-host:refs/remotes/origin/agent/other-host"],
		"the remote's agent-branch HEAD must NOT become the upstream: %v", rc.Fetch)
}

func mustRun(t *testing.T, dir, cmd string, args ...string) {
	t.Helper()
	c := exec.Command(cmd, args...)
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", cmd, args, err, out)
	}
}
