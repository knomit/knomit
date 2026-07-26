package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store/migrate"
)

// The issue-33 repro, against the REAL embedded migrations rather than the
// synthetic set in internal/store/migrate: an interrupted migration leaves
// schema_migrations.dirty = 1, every later open fails, and Manager.Add drops
// the repo — so the user's knowledge base vanishes from the API with only a
// log line. migrate.All must recover instead of failing.
func TestMigrate_RecoversFromDirtyVersion(t *testing.T) {
	dir := t.TempDir()
	registerVec() // migrations 000002/000009 need vec0
	path := filepath.Join(dir, "m.db") + "?_foreign_keys=1"

	db, err := sql.Open("sqlite3_knomit", path)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.All(db))

	var applied int
	require.NoError(t, db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&applied))

	// sqlite3 $KNOMIT_HOME/repos/<repo>.db "UPDATE schema_migrations SET dirty = 1;"
	_, err = db.Exec(`UPDATE schema_migrations SET dirty = 1`)
	require.NoError(t, err)

	require.NoError(t, migrate.All(db), "a dirty database must self-heal, not fail the open")

	var version int
	var dirty bool
	require.NoError(t, db.QueryRow(
		`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	require.Equal(t, applied, version, "recovery must not rewind the schema version")
	require.False(t, dirty, "dirty flag must be cleared")

	// The schema is still whole: the top migration's unique index survived the
	// rewind-and-re-apply, so the recovery did not leave a half-migrated store.
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='ux_edges_merge_identity'`,
	).Scan(&n))
	require.Equal(t, 1, n)

	// And a second open is a plain no-op — recovery is not sticky.
	require.NoError(t, migrate.All(db))
}
