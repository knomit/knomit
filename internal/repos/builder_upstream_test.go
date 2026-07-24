package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestOpenGit_UpstreamBranchSurvivesReboot pins that the resolved upstream
// branch is recovered from the stored remote record on every boot after the
// first.
//
// upstreamMain is written only by initDefaultGit, which runs on the FIRST boot
// alone (when OpenRepo fails). Every later boot left it empty, and ensureBranch
// then passed the literal "main" to SetRemote — an unconditional INSERT OR
// REPLACE — silently rewriting the persisted upstream and the git fetch refspec
// for any repo whose origin tracks something else. setupIndex read the same
// empty value and aimed the startup index sync at a "main" branch that does not
// exist, abandoning the real upstream.
func TestOpenGit_UpstreamBranchSurvivesReboot(t *testing.T) {
	dir := t.TempDir()

	// A bare remote whose default branch is master, seeded with a real commit
	// so ls-remote reports it and resolveOriginUpstream can detect it.
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
		Git:  config.GitConfig{Origin: "file://" + remoteDir},
		// A filesystem origin is only permitted inside LocalOriginRoot.
		LocalOriginRoot: dir,
	}

	// storedUpstream boots a Manager against cfg, reads the persisted upstream
	// branch, and shuts down again — one machine start/stop cycle.
	storedUpstream := func() string {
		m := New(context.Background(), Deps{
			Cfg:                   cfg,
			AgentBranch:           "agent/test-abc",
			DisableBackgroundSync: true,
		})
		require.NoError(t, m.Start())
		defer func() { _ = m.Close() }()

		remote, err := testService(t, m.Get(config.DefaultRepoName)).Remote().GetRemote("origin")
		require.NoError(t, err)
		require.NotNil(t, remote, "default repo with an origin must have a remote record")
		return remote.Branch
	}

	require.Equal(t, "master", storedUpstream(),
		"first boot must record the remote's real default branch")
	require.Equal(t, "master", storedUpstream(),
		`reboot must not overwrite the stored upstream with the "main" fallback`)
}
