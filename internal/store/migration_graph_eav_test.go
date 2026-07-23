package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store/migrate"
)

// A v4 database stored `deleted` as INTEGER 0/1 in node_props_bool — the
// GraphQLite extension routed properties into typed tables by storage class,
// and `deleted` was the only key ever written to the bool one. The direct-SQL
// readers only see node_props_text, so migration 000014 must CONVERT before it
// drops, or every retracted fact reads as live until a Rebuild regenerates the
// graph — and that Rebuild runs later, in the background, and can fail.
func openMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	registerVec() // migrations 000002/000009 need vec0
	db, err := sql.Open("sqlite3_knomit", filepath.Join(dir, "m.db")+"?_foreign_keys=1")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.All(db))
	return db
}

// seedV4Graph rewinds a migrated database to the v4 layout: the typed property
// tables present and populated, schema_migrations rolled back so 000014 runs
// again over them.
func seedV4Graph(t *testing.T, db *sql.DB) (liveNode, deletedNode int64) {
	t.Helper()

	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS node_props_bool (
		node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
		key_id  INTEGER NOT NULL REFERENCES property_keys(id),
		value   INTEGER NOT NULL CHECK (value IN (0, 1)),
		PRIMARY KEY (node_id, key_id))`)
	require.NoError(t, err)

	mk := func() int64 {
		res, err := db.Exec(`INSERT INTO nodes DEFAULT VALUES`)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO node_labels(node_id, label) VALUES (?, 'Fact')`, id)
		require.NoError(t, err)
		return id
	}
	liveNode, deletedNode = mk(), mk()

	_, err = db.Exec(`INSERT OR IGNORE INTO property_keys(key) VALUES ('deleted')`)
	require.NoError(t, err)
	var keyID int64
	require.NoError(t, db.QueryRow(`SELECT id FROM property_keys WHERE key='deleted'`).Scan(&keyID))

	// Exactly what the extension wrote: 0 for live, 1 for retracted.
	_, err = db.Exec(`INSERT INTO node_props_bool(node_id, key_id, value) VALUES (?, ?, 0), (?, ?, 1)`,
		liveNode, keyID, deletedNode, keyID)
	require.NoError(t, err)

	// Roll the recorded version back so 000014 replays against the v4 layout.
	_, err = db.Exec(`UPDATE schema_migrations SET version = 13, dirty = 0`)
	require.NoError(t, err)
	return liveNode, deletedNode
}

func TestMigration000014_ConvertsDeletedBeforeDroppingTypedTables(t *testing.T) {
	db := openMigratedTestDB(t)
	live, deleted := seedV4Graph(t, db)

	require.NoError(t, migrate.All(db))

	readDeleted := func(nodeID int64) string {
		var v string
		require.NoError(t, db.QueryRow(`
			SELECT p.value FROM node_props_text p
			JOIN property_keys k ON k.id = p.key_id AND k.key = 'deleted'
			WHERE p.node_id = ?`, nodeID).Scan(&v))
		return v
	}
	require.Equal(t, "false", readDeleted(live), "live node must survive as TEXT 'false'")
	require.Equal(t, "true", readDeleted(deleted),
		"retracted node must survive as TEXT 'true' — otherwise it reads as live")

	// The typed tables are gone once their contents have been carried over.
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='node_props_bool'`).Scan(&n))
	require.Zero(t, n, "node_props_bool must be dropped after conversion")
}

// A fresh database never had the typed tables. The conversion must still be a
// clean no-op there (the shim CREATE TABLE is what makes the SELECT parse).
func TestMigration000014_FreshDatabaseMigratesCleanly(t *testing.T) {
	db := openMigratedTestDB(t)

	for _, tbl := range []string{"node_props_bool", "node_props_int", "node_props_real", "node_props_json"} {
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n))
		require.Zero(t, n, "%s must not exist on a fresh database", tbl)
	}
	var deletedRows int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM node_props_text p
		JOIN property_keys k ON k.id = p.key_id AND k.key = 'deleted'`).Scan(&deletedRows))
	require.Zero(t, deletedRows, "nothing to convert on a fresh database")
}
