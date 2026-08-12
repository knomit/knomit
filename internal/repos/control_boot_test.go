package repos

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/segmentio/ksuid"
	"github.com/stretchr/testify/require"
)

func openControlDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n))
	return n > 0
}

// writePreUIDControlDB builds a control.db in the post-migrate-registry,
// pre-uid shape: membership already uid-keyed (lenses.write_uid,
// lens_reads.repo_uid), the lens row itself still keyed by name. It holds one
// repo and one lens with one read mount, and returns the repo's uid.
//
// This shape PASSES the unmigrated-home guard — no lenses.write_repo, no
// archive directory, and the repos table exists — which is what lets a test
// drive it through the real Manager.Start.
func writePreUIDControlDB(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)

	uid := ksuid.New().String()
	_, err = db.Exec(`
CREATE TABLE repos (
    uid TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'code', repo_id TEXT,
    created_at INTEGER NOT NULL, archived_at INTEGER);
CREATE TABLE lenses (
    name TEXT PRIMARY KEY, write_uid TEXT NOT NULL REFERENCES repos(uid),
    description TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo_uid TEXT NOT NULL REFERENCES repos(uid),
    branch TEXT NOT NULL DEFAULT '', source TEXT,
    PRIMARY KEY (lens_name, repo_uid));`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO repos (uid, name, state, profile, created_at)
	                  VALUES (?, 'alpha', 'active', 'code', 1)`, uid)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO lenses (name, write_uid, created_at, updated_at)
	                  VALUES ('work', ?, 1, 1)`, uid)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO lens_reads (lens_name, repo_uid) VALUES ('work', ?)`, uid)
	require.NoError(t, err)
	return uid
}

// requireReKeyed asserts the 'work' lens came back uid-keyed with its read
// mount intact — the observable proof that the re-key ran and ran first.
func requireReKeyed(t *testing.T, lenses []Lens, repoUID string) {
	t.Helper()
	require.Len(t, lenses, 1)
	require.Equal(t, "work", lenses[0].Name)
	require.NotEmpty(t, lenses[0].UID, "the lens must have been re-keyed onto a uid")
	require.Len(t, lenses[0].Reads, 1, "the read mount must survive the re-key")
	require.Equal(t, repoUID, lenses[0].Reads[0].RepoUID)
}

// The guard must run before the migrator. If it did not, schema_migrations
// would exist after a refused boot -- and creating `repos` would destroy the
// SchemaExisted evidence the guard depends on, so the refusal would happen on
// the first boot only.
//
// Driven through Manager.Start, not through refuseUnmigratedHome directly: the
// ordering under test lives in Start, so a test that called the two in its own
// preferred order could only fail if someone edited the test.
func TestBootGuard_RunsBeforeMigrator(t *testing.T) {
	m := newTestManager(t)
	home := m.deps.Cfg.Home
	path := filepath.Join(home, "control.db")

	// A genuinely pre-registry home: name-keyed lenses with write_repo.
	db := openControlDB(t, path)
	_, err := db.Exec(`CREATE TABLE lenses (
	    name TEXT PRIMARY KEY, write_repo TEXT NOT NULL,
	    description TEXT NOT NULL DEFAULT '',
	    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`)
	require.NoError(t, err)

	err = m.Start()
	require.Error(t, err, "a legacy home must be refused")
	require.Contains(t, err.Error(), "migrate-registry")

	require.False(t, hasTable(t, db, "schema_migrations"),
		"the migrator must not have run: the guard comes first")
	require.False(t, hasTable(t, db, "repos"),
		"creating repos would destroy the SchemaExisted evidence")

	// And the guard is still armed for the next attempt, which is the whole
	// point of writing nothing before it.
	reg, err := OpenRegistryNoSchema(path)
	require.NoError(t, err)
	defer reg.Close()
	require.False(t, reg.SchemaExisted())
}

// The lens re-key must run BEFORE the baseline. ALTER TABLE ... RENAME TO does
// not rename attached indexes, so a baseline that created lenses_name first
// would make the re-key's own CREATE UNIQUE INDEX lenses_name collide.
//
// Driven through Manager.Start so that reversing the two calls in the
// PRODUCTION code fails this test. A test that ran the two in its own body
// would assert only its own ordering.
func TestLensRekey_RunsBeforeBaseline(t *testing.T) {
	m := newTestManager(t)
	home := m.deps.Cfg.Home
	require.NoError(t, os.MkdirAll(home, 0o755))
	uid := writePreUIDControlDB(t, filepath.Join(home, "control.db"))

	// The pre-uid home boots: the guard passes on this shape, and Start's
	// controlUp re-keys it. A repo row whose .db is missing is an ordinary
	// unavailable repo, not a boot failure.
	require.NoError(t, m.Start())

	lenses, err := m.LensRegistry().List()
	require.NoError(t, err)
	requireReKeyed(t, lenses, uid)

	// The lens registry borrows the repo registry's handle, so its Close must
	// not shut a connection the manager is still using.
	require.NoError(t, m.LensRegistry().Close(), "a non-owning wrapper's Close is a no-op")
	require.NoError(t, m.Repos().DB().Ping(), "Close must not have shut the shared handle")
}

// A GENUINELY pre-registry home (lenses.write_repo) is the one shape controlUp
// must NOT re-key: upgradeLensSchema copies write_uid, which that home has no
// column for, so trying turns a home migrate-registry could still convert into
// a failed open. Skipping must leave the legacy evidence intact — `knomit
// migrate-registry` is the only converter, and the boot guard has to keep
// refusing this home until it has run.
func TestOpenRegistry_LeavesAPreRegistryHomeForMigrateRegistry(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	path := filepath.Join(home, "control.db")
	db := openControlDB(t, path)
	_, err := db.Exec(`CREATE TABLE lenses (
	    name TEXT PRIMARY KEY, write_repo TEXT NOT NULL,
	    description TEXT NOT NULL DEFAULT '',
	    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`)
	require.NoError(t, err)

	reg, err := OpenRegistry(path)
	require.NoError(t, err, "a legacy home must still open, not fail on a re-key it cannot do")
	defer reg.Close()
	empty, err := reg.IsEmpty()
	require.NoError(t, err)
	require.True(t, empty)

	// The legacy lens table is untouched, so the guard's first arm still fires
	// and this home cannot boot until migrate-registry has converted it.
	legacy, err := HasLegacyLensSchema(db)
	require.NoError(t, err)
	require.True(t, legacy, "the re-key must not have half-converted a migrate-registry home")
	require.Error(t, refuseUnmigratedHome(reg, reposDir))
}

// Every opener re-keys, not just the lens one. A bare migrate.Control against a
// pre-uid home creates lenses_name on the still-name-keyed table AND stamps the
// home at version 1, so the collision it sets up is PERMANENT: the next open —
// including a correctly ordered Manager.Start — dies with "index lenses_name
// already exists". OpenRegistry is a repo-registry opener that has no interest
// in lenses, which is exactly why it must not be the one that bricks them.
func TestOpenRegistry_ReKeysLensesForLaterOpeners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	uid := writePreUIDControlDB(t, path)

	reg, err := OpenRegistry(path)
	require.NoError(t, err)
	require.NoError(t, reg.Close())

	lensReg, err := OpenLensRegistry(path)
	require.NoError(t, err, "OpenRegistry must not have left a home a lens open cannot survive")
	defer lensReg.Close()
	lenses, err := lensReg.List()
	require.NoError(t, err)
	requireReKeyed(t, lenses, uid)
}
