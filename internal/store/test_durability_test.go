package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSetTestFastDurability_SyncOffWALKept pins what the #365 switch does and,
// as importantly, what it leaves alone. With it off (this package's TestMain
// never turns it on) the main DB runs synchronous=NORMAL and control.db's DSN
// gets nothing appended — production durability, and how this package's
// TestMain leaves it. With it on, every DB a test
// binary opens runs synchronous=OFF, and every one is STILL in WAL: the
// journal mode decides locking, which concurrency tests rely on, so a switch
// that quietly changed it would invalidate them without failing them.
//
// It flips a process-wide switch, so it must not run in parallel.
func TestSetTestFastDurability_SyncOffWALKept(t *testing.T) {
	// Restore whatever TestMain set, so the rest of the binary runs as it
	// asked to whether or not this package enables the switch.
	prev := testFastDurability.Load()
	t.Cleanup(func() { SetTestFastDurability(prev) })

	// synchronous: 0 = OFF, 1 = NORMAL.
	pragmas := func(t *testing.T, db *sql.DB) (journal string, sync int) {
		t.Helper()
		require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&journal))
		require.NoError(t, db.QueryRow("PRAGMA synchronous").Scan(&sync))
		return journal, sync
	}

	for _, tc := range []struct {
		name     string
		fast     bool
		wantSync int
		wantDSN  string
	}{
		{name: "off (production)", fast: false, wantSync: 1, wantDSN: ""},
		{name: "on", fast: true, wantSync: 0, wantDSN: "&_synchronous=OFF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetTestFastDurability(tc.fast)
			dir := t.TempDir()

			svc, err := Open(filepath.Join(dir, "k.db"))
			require.NoError(t, err)
			defer svc.Close()
			journal, sync := pragmas(t, svc.rh.db)
			require.Equal(t, "wal", journal, "main DB must stay in WAL")
			require.Equal(t, tc.wantSync, sync, "main DB synchronous")

			// The session sidecar is synchronous=OFF regardless; it must stay WAL.
			journal, sync = pragmas(t, svc.sessionDB)
			require.Equal(t, "wal", journal, "session DB must stay in WAL")
			require.Equal(t, 0, sync, "session DB synchronous")

			// control.db is opened by internal/repos with the stock driver and
			// this DSN shape; SyncDSNParam is all that differs.
			require.Equal(t, tc.wantDSN, SyncDSNParam())
			ctl, err := sql.Open("sqlite3", filepath.Join(dir, "control.db")+"?_busy_timeout=5000&_journal_mode=WAL"+SyncDSNParam())
			require.NoError(t, err)
			defer ctl.Close()
			ctl.SetMaxOpenConns(1)
			journal, sync = pragmas(t, ctl)
			require.Equal(t, "wal", journal, "control.db must stay in WAL")
			require.Equal(t, tc.wantSync, sync, "control.db synchronous")
		})
	}
}
