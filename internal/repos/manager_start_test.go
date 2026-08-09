package repos

import (
	"database/sql"
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

// Purge (lifecycle.go) deletes a repo's registry row before its database
// file, by design: "a failed unlink leaves an orphan file — logged at next
// Start, harmless, deletable by hand." That orphan can leave a fully migrated
// home with zero registry rows and a stray .db — the same shape as an
// unmigrated home on the surface. It must still boot: the registry table
// already existed here, purging simply emptied it.
func TestStart_PurgeOrphanDoesNotTripUnmigratedGuard(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	_, err := m.Archive("core")
	require.NoError(t, err)
	require.NoError(t, m.Purge(uid))

	// Simulate the documented failure mode: Purge's row-delete succeeded (the
	// table is empty) but its file-unlink did not, leaving this behind.
	require.NoError(t, os.WriteFile(m.RepoPath(uid), []byte("orphan"), 0o644))
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	err = m2.Start()
	require.NoError(t, err, "a purge-orphaned .db on an already-migrated home must not trip the unmigrated-home guard")
	require.Empty(t, m2.Names())
}

// Restores, on an already-migrated home, the exact fixture
// TestStart_OrphanFileIsIgnored used before the boot guard existed: zero
// registry rows plus a stray .db. The registry TABLE already exists here
// (Start created it on the first boot below), so this must boot — unlike
// TestStart_RefusesUnmigratedHome, where the table itself is created fresh
// during the failing Start call.
func TestStart_EmptyRegistryTableToleratesOrphan(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start()) // creates control.db and the repos table, zero rows
	require.NoError(t, m.Close())

	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	require.NoError(t, os.WriteFile(filepath.Join(reposDir, "orphan.db"), []byte("x"), 0o644))

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	err := m2.Start()
	require.NoError(t, err, "an orphan .db on a home whose registry table already exists must not be fatal")
	require.Empty(t, m2.Names())
}

// writeLegacyLensTables writes the PRE-registry, name-keyed lens schema into a
// control.db that has no `repos` table — the shape migrate-registry converts.
func writeLegacyLensTables(t *testing.T, controlPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", controlPath)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE lenses (
    name        TEXT PRIMARY KEY,
    write_repo  TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo      TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo)
);
INSERT INTO lenses (name, write_repo, created_at, updated_at) VALUES ('workspace', 'legacy', 1, 1);`)
	require.NoError(t, err)
}

// The guard used to fire exactly ONCE. OpenRegistry probes for the `repos`
// table and then commits it unconditionally, so the boot that refuses is also
// the boot that destroys the evidence: retry and the table exists,
// SchemaExisted is true, and the server comes up on an unconverted home
// with every legacy .db invisible. Under systemd Restart=on-failure or a Docker
// restart policy nobody ever sees the refusal.
func TestStart_RefusesUnmigratedHomeOnEveryAttempt(t *testing.T) {
	m := newTestManager(t)
	home := m.deps.Cfg.Home
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(reposDir, "legacy.db"), []byte("x"), 0o644))
	writeLegacyLensTables(t, filepath.Join(home, "control.db"))

	err := m.Start()
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrate-registry")
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = home
	err = m2.Start()
	require.Error(t, err, "the guard must not be disarmed by the boot it fired on")
	require.Contains(t, err.Error(), "migrate-registry")
	require.NoError(t, m2.Close())

	// ...and again, because "twice" is not the property being claimed.
	m3 := newTestManager(t)
	m3.deps.Cfg.Home = home
	require.Error(t, m3.Start())
}

// The commonest legacy home of all — repos/<name>.db files, no lenses ever
// created, no archive directory — is carried by the stray-file arm ALONE. The
// lens arm cannot help: there are no legacy lens tables to find, so it reports
// false truthfully. That leaves an arm whose evidence is "the repos table has
// never existed here", and a boot that creates that table on its way to
// checking destroys it. This home refused once, then booted with every repo
// invisible.
func TestStart_RefusesLenslessUnmigratedHomeOnEveryAttempt(t *testing.T) {
	m := newTestManager(t)
	home := m.deps.Cfg.Home
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(reposDir, "legacy.db"), []byte("x"), 0o644))

	for attempt := 1; attempt <= 3; attempt++ {
		mn := newTestManager(t)
		mn.deps.Cfg.Home = home
		err := mn.Start()
		require.Errorf(t, err, "boot %d must refuse: nothing between attempts converts this home", attempt)
		require.Contains(t, err.Error(), "migrate-registry")
		require.NoError(t, mn.Close())
	}

	// And the refusal is non-destructive: a refused boot writes nothing to
	// control.db, so migrate-registry still finds the home it expects — and the
	// next attempt still finds the evidence.
	reg, err := OpenRegistryNoSchema(filepath.Join(home, "control.db"))
	require.NoError(t, err)
	defer reg.Close()
	require.False(t, reg.SchemaExisted(),
		"a refused boot must not create the repos table; doing so disarms the guard it just fired")
}

// A legacy home whose repos are ALL archived has an empty repos/ and a
// populated repos/archive/. anyRepoDBFile globs one directory only, so the
// stray-file arm sees nothing and the server boots — after which every lens
// endpoint fails with a raw "no such column: write_uid", because the legacy
// `lenses` table survives CREATE TABLE IF NOT EXISTS untouched.
func TestStart_RefusesUnmigratedHomeWithOnlyArchivedRepos(t *testing.T) {
	m := newTestManager(t)
	archiveDir := filepath.Join(m.deps.Cfg.Home, "repos", "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(archiveDir, "2Nq8vXbLKZmRt3wYc7dHfGjPqAs.db"), []byte("x"), 0o644))

	err := m.Start()
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrate-registry")
	require.Contains(t, err.Error(), "archive")
}

// An archived repo's database stays at RepoPath(uid) — Archive is a state flip
// and Restore reopens the file in place. Start must therefore count archived
// uids as registered when it looks for orphans, or every archived repo is
// reported as a stray file the operator is invited to delete: precisely the
// file a restore needs.
//
// Found by running the migration tool against a real home, where two archived
// repos came back as "database file is not in the registry and will be ignored".
func TestStart_ArchivedRepoFileIsNotAnOrphan(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	_, err := m.Archive("core")
	require.NoError(t, err)
	require.FileExists(t, m.RepoPath(uid), "archive must not move the file")
	require.NoError(t, m.Close())

	m2 := newTestManager(t)
	m2.deps.Cfg.Home = m.deps.Cfg.Home
	require.NoError(t, m2.Start())

	// The repo is archived, so it has no live instance...
	require.Empty(t, m2.Names())
	// ...but Start must have counted its uid as registered, not orphaned it.
	require.NotContains(t, m2.OrphanFiles(), uid+".db",
		"an archived repo's database is registered; reporting it as an orphan "+
			"invites deleting the file a restore needs")

	// And it is still restorable from that file.
	restored, err := m2.Restore(uid, "")
	require.NoError(t, err)
	require.NotNil(t, restored)
	require.Equal(t, []string{"core"}, m2.Names())
}
