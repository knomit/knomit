package repos

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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

// TestCreate_ClonePrefersMainOverAgentBranchHEAD pins the prefer-main rule ON
// THE REPOS CLONE PATH, which is where it can actually be lost.
//
// store.InitFromRemote applies the rule itself, but ONLY when the caller passes
// an empty branch. initClone deliberately resolves the branch up front — so the
// clone, the git refspecs and the remotes row all name one branch — and
// therefore passes it in NON-empty, sailing straight past that guard. Detection
// has to carry the rule too, or a remote whose symbolic HEAD points at an agent
// branch (a GitHub default-branch misconfiguration) silently becomes our
// consensus upstream: wrong fetch refspecs, wrong remotes row, and every later
// boot faithfully reads the wrong value back.
//
// The store-level test for this (TestInitFromRemote_PrefersMainOverAgentBranchHEAD)
// calls the store directly with "" and so cannot see this path at all.
func TestCreate_ClonePrefersMainOverAgentBranchHEAD(t *testing.T) {
	dir := t.TempDir()

	remoteDir := filepath.Join(dir, "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", remoteDir)

	work := filepath.Join(dir, "work")
	runGit(t, "", "init", "--initial-branch=main", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed\n"), 0o644))
	runGit(t, work, "add", "seed.txt")
	runGit(t, work, "commit", "-m", "seed main")
	runGit(t, work, "remote", "add", "origin", remoteDir)
	runGit(t, work, "push", "origin", "main")

	// An agent branch, with the remote's HEAD pointed at it — the misconfiguration.
	runGit(t, work, "checkout", "-B", "agent/other-host")
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644))
	runGit(t, work, "add", "a.txt")
	runGit(t, work, "commit", "-m", "agent work")
	runGit(t, work, "push", "origin", "agent/other-host")
	runGit(t, remoteDir, "symbolic-ref", "HEAD", "refs/heads/agent/other-host")

	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home:            filepath.Join(dir, "home"),
			LocalOriginRoot: dir,
		},
		AgentBranch:           "agent/test-abc",
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())

	// No branch requested: the clone path must resolve one, and must not adopt
	// the agent branch the remote's HEAD names.
	ri, err := m.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)

	remote, err := testService(t, ri).Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, remote)
	require.Equal(t, "main", remote.Branch,
		"an agent branch must never become the consensus upstream when the remote advertises main")
}

// TestOpenGit_WarnsOnStoredUpstreamThatDoesNotExist covers the repos left
// damaged by the pre-fix clone path (see warnIfUpstreamMissing).
//
// Those repos persist an upstream branch they never bootstrapped, and
// resolveUpstreamMain faithfully reads the wrong value back — the prefer-main
// fix only helps NEW clones. There is no safe automatic repair, so the contract
// is that the inconsistency is LOUD rather than silent: a repo whose sync
// quietly never converges is the failure this diagnostic exists to prevent.
func TestOpenGit_WarnsOnStoredUpstreamThatDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", remoteDir)

	work := filepath.Join(dir, "work")
	runGit(t, "", "init", "--initial-branch=main", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed\n"), 0o644))
	runGit(t, work, "add", "seed.txt")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", remoteDir)
	runGit(t, work, "push", "origin", "main")

	cfg := config.Config{Home: filepath.Join(dir, "home"), LocalOriginRoot: dir}
	newManager := func() *Manager {
		return New(context.Background(), Deps{
			Cfg:                   cfg,
			AgentBranch:           "agent/test-abc",
			DisableBackgroundSync: true,
		})
	}

	first := newManager()
	require.NoError(t, first.Start())
	ri, err := first.Create(context.Background(), CreateSpec{
		Name:   testRepoName,
		Mode:   "clone",
		Origin: &OriginSpec{URL: "file://" + remoteDir},
	}, nil)
	require.NoError(t, err)
	// Reproduce the damage the old clone path did: record an upstream this
	// database never bootstrapped a branch for.
	require.NoError(t, testService(t, ri).Remote().SetUpstreamBranch("origin", "master", "agent/test-abc"))
	require.NoError(t, first.Close())

	var buf bytes.Buffer
	orig := log.Logger
	log.Logger = zerolog.New(zerolog.SyncWriter(&buf))
	t.Cleanup(func() { log.Logger = orig })

	second := newManager()
	require.NoError(t, second.Start())
	t.Cleanup(func() { _ = second.Close() })
	require.NotNil(t, second.Get(testRepoName), "the repo still opens; this is a diagnostic, not a boot failure")

	out := buf.String()
	require.Contains(t, out, "does not exist in this repo",
		"a stored upstream with no local branch must be reported, not read back in silence: %s", out)
	require.Contains(t, out, `"level":"error"`, "must be ERROR: sync silently never converges")
	require.Contains(t, out, "/origin", "must name the repair")
}
