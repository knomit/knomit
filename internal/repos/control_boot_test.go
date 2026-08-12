package repos

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/segmentio/ksuid"
	"github.com/stretchr/testify/require"

	storemigrate "knomit/internal/store/migrate"
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

// The guard must run before the migrator. If it did not, schema_migrations
// would exist after a refused boot -- and creating `repos` would destroy the
// SchemaExisted evidence the guard depends on, so the refusal would happen on
// the first boot only.
func TestBootGuard_RunsBeforeMigrator(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	require.NoError(t, os.MkdirAll(reposDir, 0o755))
	path := filepath.Join(home, "control.db")

	// A genuinely pre-registry home: name-keyed lenses with write_repo.
	db := openControlDB(t, path)
	_, err := db.Exec(`CREATE TABLE lenses (
	    name TEXT PRIMARY KEY, write_repo TEXT NOT NULL,
	    description TEXT NOT NULL DEFAULT '',
	    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`)
	require.NoError(t, err)

	reg, err := OpenRegistryNoSchema(path)
	require.NoError(t, err)
	defer reg.Close()

	err = refuseUnmigratedHome(reg, reposDir)
	require.Error(t, err, "a legacy home must be refused")
	require.Contains(t, err.Error(), "migrate-registry")

	require.False(t, hasTable(t, db, "schema_migrations"),
		"the migrator must not have run: the guard comes first")
	require.False(t, hasTable(t, db, "repos"),
		"creating repos would destroy the SchemaExisted evidence")
}

// The lens re-key must run BEFORE the baseline. ALTER TABLE ... RENAME TO does
// not rename attached indexes, so a baseline that created lenses_name first
// would make the re-key's own CREATE UNIQUE INDEX lenses_name collide.
func TestLensRekey_RunsBeforeBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db := openControlDB(t, path)

	// Post-migrate-registry but pre-uid: membership already uid-keyed,
	// the lens row itself still keyed by name.
	uid := ksuid.New().String()
	_, err := db.Exec(`
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

	// This is the boot order. Reversing the two calls is what the test guards.
	require.NoError(t, upgradeLensSchema(db))
	require.NoError(t, storemigrate.Control(db))

	lensReg := NewLensRegistry(db)
	lenses, err := lensReg.List()
	require.NoError(t, err)
	require.Len(t, lenses, 1)
	require.Equal(t, "work", lenses[0].Name)
	require.NotEmpty(t, lenses[0].UID, "the lens must have been re-keyed onto a uid")
	require.Len(t, lenses[0].Reads, 1, "the read mount must survive the re-key")
	require.Equal(t, uid, lenses[0].Reads[0].RepoUID)

	require.NoError(t, lensReg.Close(), "a non-owning wrapper's Close is a no-op")
	require.NoError(t, db.Ping(), "Close must not have shut the shared handle")
}
