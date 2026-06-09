// Tests for the configurable-upstream-branch fix: SetRemote must accept an
// explicit upstream branch name (not hardcoded "main"), configureRemote must
// build the fetch refspec from that name, reconcileMain must operate on it,
// and InitFromRemote must detect the remote symbolic HEAD when the caller
// did not pick a branch.
package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestSetRemote_PersistsUpstreamBranch is the foundational regression test.
// SetRemote must round-trip the upstream branch — not silently overwrite it
// with "main" (the bug this fix addresses).
func TestSetRemote_PersistsUpstreamBranch(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	require.NoError(t, svc.Remote().SetRemote(
		"origin", "https://example.com/repo.git",
		"master",     // upstream consensus branch
		"agent/test", // local agent branch
		300, 300, "", "",
	))

	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "master", got.Branch,
		"Remote.Branch must round-trip the upstream the caller supplied — not silently rewritten to \"main\"")
}

// TestConfigureRemote_RefspecUsesConfiguredUpstream: when SetRemote is given
// upstreamMain="master", the git config's fetch refspec must reference master
// (otherwise fetch would silently miss origin/master).
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

	// Empty upstreamMain → detection must find "master".
	require.NoError(t, svc.InitFromRemote("file://"+bareDir, nil, "", "agent/test", nil))

	got, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("master"))
	require.NoError(t, err, "local master must exist after InitFromRemote detection")
	require.NotEqual(t, plumbing.ZeroHash, got.Hash())

	// Local "main" must NOT have been created.
	_, mainErr := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("main"))
	require.ErrorIs(t, mainErr, plumbing.ErrReferenceNotFound,
		"local main must not be created when upstream is master")
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

