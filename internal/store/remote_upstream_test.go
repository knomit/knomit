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
	"strings"
	"testing"
	"time"

	"knomit/internal/platform/fileuri"

	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
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

	// Bound the fetch. Open leaves netTimeout at 0, which netCtxWith reads as
	// "no deadline", so the bogus-URL fetch below took however long the host's
	// resolver needed to give up — 0.3s on a dev box, but over the 5s resume
	// budget on a CI runner whose resolver does not fail .invalid fast. The
	// timeout makes the second phase's wait a bound rather than a hope. It
	// cannot weaken the first phase: netCtxWith runs INSIDE the closure, after
	// the RLock, so no deadline is ticking while Sync is parked. Worst case
	// fetchOrigin spends two of these (strict refspec, then the upstream-only
	// fallback), still an order of magnitude inside the budget.
	svc.SetNetworkTimeout(250 * time.Millisecond)

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
	upstream, wasEmpty, err := svc.InitFromRemote(fileuri.New(bareDir), nil, "", "agent/test", nil)
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
	upstream, wasEmpty, err := svc.InitFromRemote(fileuri.New(bareDir), nil, "", "agent/test", nil)
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

// seedBareRemoteOnMain builds a bare remote with one commit on main and returns
// its path. Shared by the subscription tests below.
func seedBareRemoteOnMain(t *testing.T) string {
	t.Helper()
	bareDir := t.TempDir()
	mustRun(t, "", "git", "init", "--bare", "--initial-branch=main", bareDir)
	work := t.TempDir()
	mustRun(t, "", "git", "clone", bareDir, work)
	mustRun(t, work, "git", "checkout", "-B", "main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644))
	mustRun(t, work, "git", "config", "user.email", "t@t")
	mustRun(t, work, "git", "config", "user.name", "t")
	mustRun(t, work, "git", "add", "seed.txt")
	mustRun(t, work, "git", "commit", "-m", "seed main")
	mustRun(t, work, "git", "push", "origin", "main")
	mustRun(t, bareDir, "git", "symbolic-ref", "HEAD", "refs/heads/main")
	return bareDir
}

func TestInitSubscription_TracksUpstreamOnlyWithNoAgentRef(t *testing.T) {
	bareDir := seedBareRemoteOnMain(t)
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	upstream, err := svc.InitSubscription(fileuri.New(bareDir), nil, "")
	require.NoError(t, err)
	require.Equal(t, "main", upstream)

	// Local main exists and equals origin/main.
	local, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("main"))
	require.NoError(t, err)
	remote, err := svc.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", "main"))
	require.NoError(t, err)
	require.Equal(t, remote.Hash(), local.Hash())

	// HEAD points at main.
	head, err := svc.rh.gits.Reference(plumbing.HEAD)
	require.NoError(t, err)
	require.Equal(t, plumbing.NewBranchReferenceName("main"), head.Target())

	// No agent ref and no watermark of any kind.
	iter, err := svc.rh.gits.IterReferences()
	require.NoError(t, err)
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		n := r.Name().String()
		require.False(t, strings.HasPrefix(n, "refs/heads/agent/"), "unexpected agent ref %s", n)
		require.False(t, strings.HasPrefix(n, "refs/knomit/"), "unexpected watermark ref %s", n)
		return nil
	}))

	// One fetch refspec.
	cfg, err := svc.rh.repo.Config()
	require.NoError(t, err)
	require.Equal(t, []gogitconfig.RefSpec{"+refs/heads/main:refs/remotes/origin/main"}, cfg.Remotes["origin"].Fetch)

	// The branches row, the branch-to-commit mapping and commit_log are all
	// populated for the followed branch.
	//
	// Asserted with SQL rather than through HeadCommit, which proves none of it:
	// HeadCommit resolves the git ref and never reads these tables, so it only
	// restates the ref check above. The gap matters because the failure is
	// silent from both ends — populateCommitLog returns nil when the ref lookup
	// fails, and InitSubscription logs and swallows its error — so a
	// subscription whose commit log never populated would still look healthy.
	ctx := context.Background()
	var branchID int64
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT id FROM branches WHERE name = ?`, "main").Scan(&branchID))

	seed := local.Hash().String()
	var onBranch int
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT count(*) FROM branch_commits WHERE branch_id = ? AND commit_hash = ?`,
		branchID, seed).Scan(&onBranch))
	require.Equal(t, 1, onBranch, "the seed commit must be mapped onto the followed branch")

	// commit_log is keyed (commit_hash, path), not by branch, so name the commit.
	var logged int
	require.NoError(t, svc.rh.db.QueryRowContext(ctx,
		`SELECT count(*) FROM commit_log WHERE commit_hash = ?`, seed).Scan(&logged))
	require.Greater(t, logged, 0, "the seed commit must be in commit_log")
}

func TestInitSubscription_EmptyRemoteIsRefused(t *testing.T) {
	bareDir := t.TempDir()
	mustRun(t, "", "git", "init", "--bare", "--initial-branch=main", bareDir)
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	_, err = svc.InitSubscription(fileuri.New(bareDir), nil, "")
	require.ErrorIs(t, err, transport.ErrEmptyRemoteRepository)
}

// Subscribing resolves the upstream by the same rule as a clone: prefer main,
// else the remote HEAD. A master-only remote yields "master".
func TestInitSubscription_ResolvesRemoteHEADWhenNoMain(t *testing.T) {
	bareDir := t.TempDir()
	mustRun(t, "", "git", "init", "--bare", "--initial-branch=master", bareDir)
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

	upstream, err := svc.InitSubscription(fileuri.New(bareDir), nil, "")
	require.NoError(t, err)
	require.Equal(t, "master", upstream)
	_, mainErr := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("main"))
	require.ErrorIs(t, mainErr, plumbing.ErrReferenceNotFound)
}
