package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"

	migrate "github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// These exercise upWithRecovery against a synthetic migration set, so each
// interruption window can be reproduced exactly. TestMigrate_Recovers... in
// package store covers the real embedded migrations end to end.

func testMigrator(t *testing.T, db *sql.DB, files fstest.MapFS) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(files, ".")
	require.NoError(t, err)
	drv, err := sqlite3.WithInstance(db, &sqlite3.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", src, "sqlite3", drv)
	require.NoError(t, err)
	return m
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "m.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func file(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(body)} }

// version reads schema_migrations directly — the point of these tests is the
// bookkeeping row, so they must not read it through the API under test.
func version(t *testing.T, db *sql.DB) (int, bool) {
	t.Helper()
	var v int
	var dirty bool
	require.NoError(t, db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty))
	return v, dirty
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n))
	return n == 1
}

// Window 1: crash BEFORE the body committed. dirty is set at 2, but table `b`
// was never created. Recovery must rewind and actually re-apply it.
func TestMigrate_RecoversFromDirtyVersion(t *testing.T) {
	files := fstest.MapFS{
		"1_a.up.sql": file(`CREATE TABLE IF NOT EXISTS a (x INT);`),
		"2_b.up.sql": file(`CREATE TABLE IF NOT EXISTS b (x INT);`),
	}
	db := testDB(t)
	require.NoError(t, upWithRecovery(testMigrator(t, db, files)))
	require.True(t, tableExists(t, db, "b"))

	// Simulate the interruption: drop the body's effect, re-dirty at 2.
	_, err := db.Exec(`DROP TABLE b`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE schema_migrations SET version = 2, dirty = 1`)
	require.NoError(t, err)

	require.NoError(t, upWithRecovery(testMigrator(t, db, files)))

	v, dirty := version(t, db)
	require.Equal(t, 2, v)
	require.False(t, dirty, "dirty flag must be cleared")
	require.True(t, tableExists(t, db, "b"), "interrupted migration must be re-applied")
}

// The first migration is the edge case: rewinding to version 0 is invalid, so
// recovery must go to NilVersion or the re-run silently does nothing.
func TestMigrate_RecoversFromDirtyFirstVersion(t *testing.T) {
	files := fstest.MapFS{"1_a.up.sql": file(`CREATE TABLE IF NOT EXISTS a (x INT);`)}
	db := testDB(t)
	require.NoError(t, upWithRecovery(testMigrator(t, db, files)))

	_, err := db.Exec(`DROP TABLE a`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE schema_migrations SET version = 1, dirty = 1`)
	require.NoError(t, err)

	require.NoError(t, upWithRecovery(testMigrator(t, db, files)))

	v, dirty := version(t, db)
	require.Equal(t, 1, v)
	require.False(t, dirty)
	require.True(t, tableExists(t, db, "a"), "migration 1 must be re-applied, not skipped")
}

// Window 2: crash AFTER the body committed but before dirty was cleared, on a
// migration that cannot be re-run (ALTER TABLE ADD COLUMN has no IF NOT EXISTS
// in SQLite — migrations 5/6/11/12 are exactly this shape). The re-run fails
// with "duplicate column name", which proves the body landed; recovery must
// accept version 2 as applied and carry on to 3.
func TestMigrate_RecoversWhenBodyAlreadyCommitted(t *testing.T) {
	files := fstest.MapFS{
		"1_a.up.sql":   file(`CREATE TABLE IF NOT EXISTS a (x INT);`),
		"2_col.up.sql": file(`ALTER TABLE a ADD COLUMN y INT;`),
		"3_c.up.sql":   file(`CREATE TABLE IF NOT EXISTS c (x INT);`),
	}
	db := testDB(t)
	require.NoError(t, upWithRecovery(testMigrator(t, db, fstest.MapFS{
		"1_a.up.sql": files["1_a.up.sql"],
	})))
	// Apply 2's body by hand, then mark it dirty: the exact state a crash
	// between the body's commit and the dirty-clearing UPDATE leaves behind.
	_, err := db.Exec(`ALTER TABLE a ADD COLUMN y INT`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE schema_migrations SET version = 2, dirty = 1`)
	require.NoError(t, err)

	require.NoError(t, upWithRecovery(testMigrator(t, db, files)))

	v, dirty := version(t, db)
	require.Equal(t, 3, v, "recovery must finish the remaining migrations")
	require.False(t, dirty)
	require.True(t, tableExists(t, db, "c"))
}

// A crash INSIDE recovery, between the rewind to N-1 and the re-run
// committing, leaves the bookkeeping CLEAN at 1 while migration 2's body is
// already applied. The next boot re-runs 2 from a clean start, so it fails with
// "duplicate column name" rather than ErrDirty — recovery must recognise that
// too, or the repo is dropped for one more boot before the flag routes it back
// through the dirty path.
func TestMigrate_RecoversFromInterruptedRecovery(t *testing.T) {
	files := fstest.MapFS{
		"1_a.up.sql":   file(`CREATE TABLE IF NOT EXISTS a (x INT);`),
		"2_col.up.sql": file(`ALTER TABLE a ADD COLUMN y INT;`),
		"3_c.up.sql":   file(`CREATE TABLE IF NOT EXISTS c (x INT);`),
	}
	db := testDB(t)
	require.NoError(t, upWithRecovery(testMigrator(t, db, fstest.MapFS{
		"1_a.up.sql": files["1_a.up.sql"],
	})))
	_, err := db.Exec(`ALTER TABLE a ADD COLUMN y INT`)
	require.NoError(t, err)
	// Note dirty = 0: the interrupted recovery had already forced back to 1.
	_, err = db.Exec(`UPDATE schema_migrations SET version = 1, dirty = 0`)
	require.NoError(t, err)

	require.NoError(t, upWithRecovery(testMigrator(t, db, files)),
		"a recovery interrupted mid-flight must complete on the next boot, not fail it")

	v, dirty := version(t, db)
	require.Equal(t, 3, v)
	require.False(t, dirty)
	require.True(t, tableExists(t, db, "c"))
}

// The re-run after a rewind replays N *and everything after it*, so an
// "already exists" can come from a later, never-applied migration. Only a
// collision on N itself proves N's body committed; a failure at 3 must be
// reported as a failure at 3, not silently forced past as if it were 2.
func TestMigrate_DoesNotForcePastAFailureInALaterMigration(t *testing.T) {
	files := fstest.MapFS{
		"1_a.up.sql": file(`CREATE TABLE IF NOT EXISTS a (x INT);`),
		"2_b.up.sql": file(`CREATE TABLE IF NOT EXISTS b (x INT);`),
		"3_z.up.sql": file(`CREATE TABLE z (x INT);`), // deliberately not idempotent
	}
	db := testDB(t)
	require.NoError(t, upWithRecovery(testMigrator(t, db, fstest.MapFS{
		"1_a.up.sql": files["1_a.up.sql"],
		"2_b.up.sql": files["2_b.up.sql"],
	})))
	// `z` exists for some reason unrelated to the interruption, so migration 3
	// will collide with it the first time it is ever applied.
	_, err := db.Exec(`CREATE TABLE z (x INT)`)
	require.NoError(t, err)

	// Interrupt 2 in window 1: body never landed, dirty at 2.
	_, err = db.Exec(`DROP TABLE b`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE schema_migrations SET version = 2, dirty = 1`)
	require.NoError(t, err)

	err = upWithRecovery(testMigrator(t, db, files))
	require.Error(t, err)
	require.Contains(t, err.Error(), "migration 3",
		"the failure must be attributed to 3, not to the recovered version 2")
	require.Contains(t, err.Error(), "already exists")

	require.True(t, tableExists(t, db, "b"), "migration 2 was still legitimately re-applied")
	v, dirty := version(t, db)
	require.Equal(t, 3, v, "3 must be left dirty as the failing migration")
	require.True(t, dirty)
}

// A migration that fails on the DATA fails identically on every retry. Recovery
// must give up after one attempt, report the underlying error rather than the
// dirty flag, and leave dirty set — a self-heal loop here would re-dirty on
// every boot and make a one-time manual recovery permanent.
func TestMigrate_DoesNotLoopOnDeterministicFailure(t *testing.T) {
	files := fstest.MapFS{
		"1_a.up.sql":  file(`CREATE TABLE IF NOT EXISTS a (x INT);`),
		"2_ux.up.sql": file(`CREATE UNIQUE INDEX IF NOT EXISTS ux_a ON a(x);`),
	}
	db := testDB(t)
	require.NoError(t, upWithRecovery(testMigrator(t, db, fstest.MapFS{
		"1_a.up.sql": files["1_a.up.sql"],
	})))
	// Duplicate rows the unique index cannot tolerate.
	_, err := db.Exec(`INSERT INTO a (x) VALUES (1), (1)`)
	require.NoError(t, err)

	err = upWithRecovery(testMigrator(t, db, files))
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNIQUE constraint failed",
		"the underlying migration error is the actionable one, not the dirty flag")

	_, dirty := version(t, db)
	require.True(t, dirty, "dirty must be left set when recovery gives up")

	// A second boot must behave identically: still an error, still no loop.
	err = upWithRecovery(testMigrator(t, db, files))
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNIQUE constraint failed")
}
