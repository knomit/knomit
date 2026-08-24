package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// H-1. lab_guard.go's scope note promises that snapshot's one touch of a live
// home is "a read and a checkpoint, no session, no migration". It has to be
// true, not aspirational: an operator reads that sentence before pointing this
// tool at their real corpus.
//
// The test is behavioural rather than a source grep. It hands
// checkpointLiveHome a database that is NOT a knomit store — one plain table,
// WAL mode — and requires it to come back with exactly that table. store.Open
// would run migrate.All and leave dozens of knomit tables behind, so restoring
// it turns this red.
func TestCheckpointLiveHome_DoesNotMigrateOrCreateASession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notaknomitstore.db")

	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE only_mine (x TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO only_mine VALUES ('a')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	before := tableNames(t, path)
	require.Equal(t, []string{"only_mine"}, before, "precondition: one table, and not a knomit schema")

	require.NoError(t, checkpointLiveHome(path))

	require.Equal(t, before, tableNames(t, path),
		"checkpointing a live home must not migrate it — this database gained tables")

	// And no session sidecar left behind next to the user's data.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), "sessions",
			"checkpointing a live home must not write a session database beside it")
	}
}

func tableNames(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	require.NoError(t, err)
	defer db.Close()
	rows, err := db.Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	require.NoError(t, rows.Err())
	return out
}
