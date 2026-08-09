package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration017_UpgradesPreMigrationDatabase runs migration 000017 against a
// database that genuinely predates it — the case every existing installation
// hits on its next boot — rather than trusting the version number.
//
// It matters twice over. The migration is a TABLE REBUILD (see the .up.sql for
// why `ALTER TABLE ... DROP COLUMN` cannot be used here), so it has to carry the
// status rows across itself; and the connection columns it drops were, until
// this change, the fallback GetRemote read when no origin was injected. Both are
// asserted below.
func TestMigration017_UpgradesPreMigrationDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build the pre-migration fixture by opening a current database and then
	// rewinding it with 000017's OWN down migration, so the shape under test is
	// the real prior schema rather than a hand-built approximation.
	svc, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	require.NoError(t, svc.Close())

	downSQL, err := os.ReadFile(filepath.Join(
		"migrate", "migrations", "000017_remotes_drop_connection.down.sql"))
	require.NoError(t, err)

	raw, err := sql.Open("sqlite3_knomit", path)
	require.NoError(t, err)
	_, err = raw.Exec(string(downSQL))
	require.NoError(t, err, "the down migration must apply to a migrated database")
	_, err = raw.Exec(`UPDATE schema_migrations SET version = 16, dirty = 0`)
	require.NoError(t, err)

	// A pre-migration row: connection identity AND status in the one row, the
	// shape SetRemote used to write.
	_, err = raw.Exec(
		`INSERT INTO remotes (name, url, branch, interval, push_interval,
		                      last_sync_at, last_status, last_error,
		                      last_push_at, last_push_status, auth_method, auth_token)
		 VALUES ('origin', 'https://legacy.test/kb.git', 'master', 900, 900,
		         '2026-01-02T03:04:05Z', 'ok', NULL,
		         '2026-01-02T03:04:06Z', 'ok', 'token', 'ciphertext')`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	// Reopen through the production path: migration 017 must run to completion.
	svc2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc2.Close() })
	require.NoError(t, svc2.OpenRepo())

	var v int
	var dirty bool
	require.NoError(t, svc2.rh.db.QueryRow(
		`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty))
	require.Equal(t, 17, v, "the store must have migrated forward")
	require.False(t, dirty)

	// The connection columns are gone...
	for _, gone := range []string{"url", "branch", "auth_method", "auth_token"} {
		var n int
		require.NoError(t, svc2.rh.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('remotes') WHERE name = ?`, gone).Scan(&n))
		require.Zero(t, n, "remotes.%s must be dropped", gone)
	}

	// ...and this repo now reports NO origin, because control.db holds it and
	// nothing was injected. The dropped columns are not a silent second source.
	got, err := svc2.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, got, "a migrated repo with no injected origin has no origin")

	// The status the rebuild had to carry across survives, and reassembles onto
	// the injected origin exactly as a never-migrated repo's would.
	svc2.SetOrigin(&Origin{URL: "https://control.test/kb.git", Branch: "master"})
	got, err = svc2.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "https://control.test/kb.git", got.URL)
	require.Equal(t, 900, got.Interval, "the pre-migration interval must survive the rebuild")
	require.Equal(t, 900, got.PushInterval)
	require.NotNil(t, got.LastSyncAt)
	require.Equal(t, "2026-01-02T03:04:05Z", *got.LastSyncAt)
	require.NotNil(t, got.LastStatus)
	require.Equal(t, "ok", *got.LastStatus)
	require.NotNil(t, got.LastPushAt)
	require.Equal(t, "2026-01-02T03:04:06Z", *got.LastPushAt)

	// And the migrated table still accepts a status upsert.
	require.NoError(t, svc2.Remote().RecordSyncError("origin", "boom"))
	got, err = svc2.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Equal(t, "error", *got.LastStatus)
	require.Equal(t, "boom", *got.LastError)
}
