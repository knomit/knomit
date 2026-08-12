package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// controlDB opens a temp control.db with the same DSN internal/repos uses.
func controlDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func objectExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n))
	return n > 0
}

func controlVersion(t *testing.T, db *sql.DB) (int, bool) {
	t.Helper()
	var v int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty))
	return v, dirty
}

// The five objects the baseline is responsible for.
var controlObjects = []string{
	"repos", "repos_active_name", "repos_active_repo_id",
	"repo_origins", "lenses", "lenses_name", "lens_reads",
}

// A fresh home gets the whole control schema and lands on version 1.
func TestControl_FreshDatabase(t *testing.T) {
	db := controlDB(t)
	require.NoError(t, Control(db))

	for _, name := range controlObjects {
		require.True(t, objectExists(t, db, name), "expected %q to exist", name)
	}
	v, dirty := controlVersion(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)
}

// An existing home already carrying the current shape is stamped, not rebuilt:
// the baseline is a no-op and every row survives.
func TestControl_NoOpOnExistingShape(t *testing.T) {
	db := controlDB(t)

	// Build the current shape by hand, the way a pre-migrate home looks.
	_, err := db.Exec(`
CREATE TABLE repos (
    uid TEXT PRIMARY KEY, name TEXT NOT NULL, state TEXT NOT NULL,
    profile TEXT NOT NULL DEFAULT 'code', repo_id TEXT,
    created_at INTEGER NOT NULL, archived_at INTEGER);
CREATE UNIQUE INDEX repos_active_name ON repos(name) WHERE state = 'active';
CREATE UNIQUE INDEX repos_active_repo_id ON repos(repo_id) WHERE state = 'active' AND repo_id IS NOT NULL;
CREATE TABLE repo_origins (
    repo_uid TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    url TEXT NOT NULL, branch TEXT NOT NULL,
    auth_method TEXT NOT NULL DEFAULT '', auth_token TEXT NOT NULL DEFAULT '');
CREATE TABLE lenses (
    uid TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL,
    write_uid TEXT NOT NULL REFERENCES repos(uid),
    description TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
CREATE UNIQUE INDEX lenses_name ON lenses(name);
CREATE TABLE lens_reads (
    lens_uid TEXT NOT NULL REFERENCES lenses(uid) ON DELETE CASCADE,
    repo_uid TEXT NOT NULL REFERENCES repos(uid),
    branch TEXT NOT NULL DEFAULT '', source TEXT,
    PRIMARY KEY (lens_uid, repo_uid));`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO repos (uid, name, state, profile, created_at)
	                  VALUES ('u1', 'alpha', 'active', 'code', 7)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO lenses (uid, name, write_uid, created_at, updated_at)
	                  VALUES ('l1', 'work', 'u1', 7, 7)`)
	require.NoError(t, err)

	require.NoError(t, Control(db))

	v, dirty := controlVersion(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)

	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM repos WHERE uid = 'u1'`).Scan(&name))
	require.Equal(t, "alpha", name)
	var lens string
	require.NoError(t, db.QueryRow(`SELECT name FROM lenses WHERE uid = 'l1'`).Scan(&lens))
	require.Equal(t, "work", lens)
}

// An interrupted control.db migration self-heals rather than wedging every
// repo behind a permanently dirty control plane (issue #33).
func TestControl_RecoversDirtyVersion(t *testing.T) {
	db := controlDB(t)
	require.NoError(t, Control(db))

	_, err := db.Exec(`UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, err)

	require.NoError(t, Control(db), "a dirty control.db must self-heal")

	v, dirty := controlVersion(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)
	require.True(t, objectExists(t, db, "repos"))
}
