package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store/migrate"
)

// TestMigrate_DropsMotifBackfillJudged pins that migration 000024 actually
// removes the table, against the REAL embedded migrations.
//
// WHY THIS NEEDS ITS OWN TEST, when every store test already migrates forward:
// migrating forward only proves 000024 did not ERROR. `DROP TABLE IF EXISTS`
// cannot error on a name that does not exist, so a typo in the table name —
// `motif_backfil_judged` — is a silent no-op that leaves the table in place and
// passes the entire suite. The IF EXISTS clause is right (the migration must be
// idempotent) and it is exactly what removes the failure signal, so the check
// has to be an assertion about the RESULT rather than about the run.
//
// The assertion is falsifiable in both directions that matter: 000022 creates
// this table, so if 000024 is removed, typo'd, or no-ops, the table is still
// here and this goes red. It cannot pass vacuously — a table that was never
// created would mean 000022 broke, which every other store test would catch.
func TestMigrate_DropsMotifBackfillJudged(t *testing.T) {
	dir := t.TempDir()
	registerVec() // migrations 000002/000009 need vec0
	path := filepath.Join(dir, "m.db") + "?_foreign_keys=1"

	db, err := sql.Open("sqlite3_knomit", path)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.All(db))

	// Fully migrated, or the absence below would say nothing about 000024.
	var applied int
	require.NoError(t, db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&applied))
	latest, err := migrate.LatestRepoVersion()
	require.NoError(t, err)
	require.Equal(t, latest, applied, "the fixture must be migrated all the way forward")

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		"motif_backfill_judged").Scan(&n))
	require.Zero(t, n,
		"motif_backfill_judged survived a full migration. 000022 creates it and "+
			"000024 must drop it; DROP TABLE IF EXISTS cannot fail on a wrong name, "+
			"so a typo there is invisible everywhere except here.")

	// A control on the query itself: a table that IS expected proves this is
	// reading the schema rather than always returning zero.
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		"facts").Scan(&n))
	require.Equal(t, 1, n, "the sqlite_master probe must be able to FIND a table")
}
