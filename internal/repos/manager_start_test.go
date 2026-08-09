package repos

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Start opens what the registry says exists, keyed by uid — not what the
// directory happens to contain.
func TestStart_OpensRegisteredRepos(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	require.NoError(t, m2.Start())
	got := m2.Get("core")
	require.NotNil(t, got)
	require.Equal(t, uid, got.UID())
}

// A registered repo whose file is gone stays VISIBLE as unavailable rather
// than vanishing from the API — the whole point of a registry that outlives
// the file.
func TestStart_MissingFileReportsUnavailable(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	home := m.deps.Cfg.Home
	require.NoError(t, m.Close())

	require.NoError(t, os.Remove(m.RepoPath(uid)))

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = home
	require.NoError(t, m2.Start())

	require.Nil(t, m2.Get("core"), "no live instance for a missing file")
	un := m2.Unavailable()
	require.Len(t, un, 1)
	require.Equal(t, "core", un[0].Record.Name)
	require.Equal(t, "missing", un[0].Reason)
}

// TestStart_ClassifiesUnavailableReasons exercises openRegistered,
// markUnavailable/clearUnavailable, and Unavailable() directly — no Task 6
// dependency, since Registry.Insert already exists and this test is
// `package repos`. It seeds two registry rows by hand (bypassing
// Manager.Create entirely) to cover two of the three Unavailable reasons:
// "missing" (no file at all) and "unopenable" (a file that isn't a valid
// store). "conflict" needs two openable repos sharing a root commit, which
// needs the mirror-clone fixture Task 7 builds — left to that task.
func TestStart_ClassifiesUnavailableReasons(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "repos"), 0o755))

	reg, err := OpenRegistry(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	const (
		missingUID    = "missing-uid"
		unopenableUID = "unopenable-uid"
	)
	require.NoError(t, reg.Insert(RepoRecord{
		UID: missingUID, Name: "zeta", State: StateActive, Profile: ProfileCode, CreatedAt: 1,
	}))
	require.NoError(t, reg.Insert(RepoRecord{
		UID: unopenableUID, Name: "alpha", State: StateActive, Profile: ProfileCode, CreatedAt: 2,
	}))
	require.NoError(t, reg.Close())

	m := newTestManager(t)
	m.deps.Cfg.Home = home

	// Case A ("missing"): zeta's uid has no file at m.RepoPath at all.
	// Case B ("unopenable"): alpha's uid has a file, but it isn't a valid
	// store — openOne must fail rather than panic or silently succeed.
	require.NoError(t, os.WriteFile(m.RepoPath(unopenableUID), []byte("not a sqlite db"), 0o644))

	require.NoError(t, m.Start())

	require.Nil(t, m.Get("zeta"), "a missing file must not produce a live instance")
	require.Nil(t, m.Get("alpha"), "an unopenable file must not produce a live instance")

	un := m.Unavailable()
	require.Len(t, un, 2)
	// Sorted by name ("alpha" before "zeta") — pins Unavailable()'s ordering
	// with more than one row.
	require.Equal(t, "alpha", un[0].Record.Name)
	require.Equal(t, "unopenable", un[0].Reason)
	require.Equal(t, "zeta", un[1].Record.Name)
	require.Equal(t, "missing", un[1].Reason)
}

// An unmigrated home must fail loudly at boot, not half-work. Deliberately not
// a filename-shape test: a repo name may legally look like a ksuid.
func TestStart_RefusesUnmigratedHome(t *testing.T) {
	m := newTestManager(t)
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(reposDir, "legacy.db"), []byte("x"), 0o644))

	err := m.Start()
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrate-registry")
}

// A fresh home with zero repos is a VALID steady state, not an unmigrated one.
func TestStart_EmptyHomeIsFine(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	require.Empty(t, m.Names())
}

// A .db with no registry row is inert. Dropping a file into repos/ is no
// longer a way to register anything.
//
// This is checked alongside a real registered repo, not on an otherwise-empty
// registry: an empty registry plus any .db file is the unmigrated-home
// signature (TestStart_RefusesUnmigratedHome) and must fail loudly instead.
func TestStart_OrphanFileIsIgnored(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	createRepo(t, m, "core")
	require.NoError(t, os.WriteFile(m.RepoPath("orphan"), []byte("not a db"), 0o644))
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	require.NoError(t, m2.Start())
	require.Equal(t, []string{"core"}, m2.Names())
	require.Empty(t, m2.Unavailable())
}
