package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Migration 000016 moves the index schema version from one global meta row to
// meta.graph_schema_version:<branch>. These exercise the migration BODY against
// a real store — the runner itself is covered by internal/store/migrate.
//
// Re-running the file on an already-migrated DB is exactly the pre-16 shape once
// the test puts the global row back, so the assertions below are about the SQL,
// not about golang-migrate's bookkeeping.

func migrationSQL(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("migrate", "repo", name))
	require.NoError(t, err)
	return string(body)
}

func twoBranchStore(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/x", "main"))
	return svc
}

// simulatePre16 rewinds the meta rows to how a pre-000016 database looked: one
// global version row, no per-branch rows.
func simulatePre16(t *testing.T, svc *Service, version string) {
	t.Helper()
	_, err := svc.si.rh.db.Exec(`DELETE FROM meta WHERE key GLOB 'graph_schema_version:*'`)
	require.NoError(t, err)
	_, err = svc.si.rh.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES ('graph_schema_version', ?)`, version)
	require.NoError(t, err)
}

func metaValue(t *testing.T, svc *Service, key string) (string, bool) {
	t.Helper()
	rows, err := svc.si.rh.db.Query(`SELECT value FROM meta WHERE key = ?`, key)
	require.NoError(t, err)
	defer rows.Close()
	if !rows.Next() {
		return "", false
	}
	var v string
	require.NoError(t, rows.Scan(&v))
	return v, true
}

// TestMigration016_CarriesGlobalVersionOntoEveryKnownBranch pins the upgrade
// path: a deployment already at the current version must NOT be forced into a
// full re-index just because the key moved. Every branch the index knows about
// inherits the global value, and the global key is dropped so nothing can fall
// back to it.
func TestMigration016_CarriesGlobalVersionOntoEveryKnownBranch(t *testing.T) {
	svc := twoBranchStore(t)
	ctx := context.Background()
	simulatePre16(t, svc, GraphSchemaVersion)

	_, err := svc.si.rh.db.Exec(migrationSQL(t, "000016_per_branch_graph_schema_version.up.sql"))
	require.NoError(t, err)

	_, ok := metaValue(t, svc, "graph_schema_version")
	require.False(t, ok, "the global key must be gone, so no reader can fall back to it")

	for _, branch := range []string{"main", "agent/x"} {
		stale, err := svc.IndexManager().NeedsRebuild(ctx, branch)
		require.NoError(t, err)
		require.False(t, stale, "%s inherited the current global version; no re-index needed", branch)
	}

	// A branch the index has never seen has no row, so it reads stale — which is
	// what a never-indexed branch needs anyway.
	stale, err := svc.IndexManager().NeedsRebuild(ctx, "agent/never-indexed")
	require.NoError(t, err)
	require.True(t, stale)
}

// TestMigration016_CarriesStaleVersionThrough pins the other half: an older
// deployment must still be seen as stale after the move, not silently marked
// current.
func TestMigration016_CarriesStaleVersionThrough(t *testing.T) {
	svc := twoBranchStore(t)
	ctx := context.Background()
	simulatePre16(t, svc, "2")

	_, err := svc.si.rh.db.Exec(migrationSQL(t, "000016_per_branch_graph_schema_version.up.sql"))
	require.NoError(t, err)

	for _, branch := range []string{"main", "agent/x"} {
		stale, err := svc.IndexManager().NeedsRebuild(ctx, branch)
		require.NoError(t, err)
		require.True(t, stale, "%s carried the old version forward and must still rebuild", branch)
	}
}

// TestMigration016_DownCollapsesOnlyWhenBranchesAgree pins the rollback: the
// global key cannot express "A current, B stale", so a disagreeing set restores
// nothing (missing reads as stale → the pre-16 code rebuilds everything, which
// is correct if wasteful). An agreeing set collapses cleanly.
func TestMigration016_DownCollapsesOnlyWhenBranchesAgree(t *testing.T) {
	down := migrationSQL(t, "000016_per_branch_graph_schema_version.down.sql")

	t.Run("agree", func(t *testing.T) {
		svc := twoBranchStore(t)
		for _, branch := range []string{"main", "agent/x"} {
			_, err := svc.si.rh.db.Exec(
				`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
				schemaVersionKey(branch), GraphSchemaVersion)
			require.NoError(t, err)
		}

		_, err := svc.si.rh.db.Exec(down)
		require.NoError(t, err)

		v, ok := metaValue(t, svc, "graph_schema_version")
		require.True(t, ok, "a unanimous set collapses to the global key")
		require.Equal(t, GraphSchemaVersion, v)
	})

	t.Run("disagree", func(t *testing.T) {
		svc := twoBranchStore(t)
		_, err := svc.si.rh.db.Exec(
			`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
			schemaVersionKey("main"), GraphSchemaVersion)
		require.NoError(t, err)
		_, err = svc.si.rh.db.Exec(
			`INSERT OR REPLACE INTO meta(key, value) VALUES (?, '2')`, schemaVersionKey("agent/x"))
		require.NoError(t, err)

		_, err = svc.si.rh.db.Exec(down)
		require.NoError(t, err)

		_, ok := metaValue(t, svc, "graph_schema_version")
		require.False(t, ok, "a split set must restore nothing rather than pick a value that lies")
	})

	// Either way the per-branch rows are gone.
	svc := twoBranchStore(t)
	_, err := svc.si.rh.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		schemaVersionKey("main"), GraphSchemaVersion)
	require.NoError(t, err)
	_, err = svc.si.rh.db.Exec(down)
	require.NoError(t, err)
	_, ok := metaValue(t, svc, schemaVersionKey("main"))
	require.False(t, ok, "per-branch rows must not survive the rollback")
}
