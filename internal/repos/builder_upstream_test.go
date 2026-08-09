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
// upstreamMain is written only by the clone (initClone, from what
// InitFromRemote resolved). Every later boot starts with it empty, and if the
// builder filled that gap with the literal "main" instead of reading the stored
// row back, SetRemote — an unconditional INSERT OR REPLACE — would silently
// rewrite the persisted upstream and the git fetch refspec for any repo whose
// origin tracks something else, while setupIndex aimed the startup index sync
// at a "main" branch that does not exist.
func TestOpenGit_UpstreamBranchSurvivesReboot(t *testing.T) {
	dir := t.TempDir()

	// A bare remote whose default branch is master, seeded with a real commit
	// so the clone's detection reports it.
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

	boot := func() (*Manager, func()) {
		m := New(context.Background(), Deps{
			Cfg:                   cfg,
			AgentBranch:           "agent/test-abc",
			DisableBackgroundSync: true,
		})
		require.NoError(t, m.Start())
		// Start opens what the registry says exists — a reboot over an
		// already-seeded home re-opens the previously created repo on its own.
		return m, func() { _ = m.Close() }
	}

	// First boot: clone the repo with NO explicit branch, so the upstream is
	// whatever the clone resolves against the remote.
	m, done := boot()
	_, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.NoError(t, err)

	storedUpstream := func(m *Manager) string {
		remote, err := testService(t, m.Get(testRepoName)).Remote().GetRemote("origin")
		require.NoError(t, err)
		require.NotNil(t, remote, "a cloned repo must have a remote record")
		return remote.Branch
	}
	require.Equal(t, "master", storedUpstream(m),
		"the clone must record the remote's real default branch")
	done()

	// Reboot over the same home: the repo is re-opened from disk, and nothing
	// in that path may overwrite the stored upstream.
	m2, done2 := boot()
	defer done2()
	require.Equal(t, "master", storedUpstream(m2),
		`reboot must not overwrite the stored upstream with the "main" fallback`)
}
