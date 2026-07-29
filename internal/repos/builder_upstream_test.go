package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestOpenGit_UpstreamBranchSurvivesReboot pins that a repo's consensus branch
// is recovered from its own stored remote record on every boot after the first.
//
// upstreamMain used to be written only by the default repo's first-run
// bootstrap. Every later boot left it empty, and ensureBranch then passed the
// literal "main" to SetRemote — an unconditional INSERT OR REPLACE — silently
// rewriting the persisted upstream and the git fetch refspec for any repo whose
// origin tracks something else. setupIndex read the same empty value and aimed
// the startup index sync at a "main" branch that does not exist, abandoning the
// real upstream.
func TestOpenGit_UpstreamBranchSurvivesReboot(t *testing.T) {
	dir := t.TempDir()

	// A bare remote whose default branch is master, seeded with a real commit
	// so ls-remote reports it and detectUpstream can detect it.
	remoteDir := filepath.Join(dir, "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch=master", remoteDir)

	work := filepath.Join(dir, "work")
	runGit(t, "", "init", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644))
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", remoteDir)
	runGit(t, work, "push", "origin", "master")

	cfg := config.Config{
		Home: filepath.Join(dir, "home"),
		// A filesystem origin is only permitted inside LocalOriginRoot.
		LocalOriginRoot: dir,
	}

	newManager := func() *Manager {
		return New(context.Background(), Deps{
			Cfg:                   cfg,
			AgentBranch:           "agent/test-abc",
			DisableBackgroundSync: true,
		})
	}

	// First boot: create the repo from the master-default origin. Branch is
	// left empty on purpose so the clone path must detect the remote's HEAD.
	first := newManager()
	require.NoError(t, first.Start())
	_, err := first.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.NoError(t, err)
	remote, err := testService(t, first.Get(testRepoName)).Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, remote, "a repo with an origin must have a remote record")
	require.Equal(t, "master", remote.Branch,
		"the create path must record the remote's real default branch")
	require.NoError(t, first.Close())

	// Re-boot against the same home: the repo comes back through the registry
	// reconcile, and its stored upstream must survive untouched.
	second := newManager()
	require.NoError(t, second.Start())
	t.Cleanup(func() { _ = second.Close() })
	ri := second.Get(testRepoName)
	require.NotNil(t, ri, "the registered repo must be reopened on re-boot")
	remote, err = testService(t, ri).Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, remote)
	require.Equal(t, "master", remote.Branch,
		`reboot must not overwrite the stored upstream with the "main" fallback`)
}
