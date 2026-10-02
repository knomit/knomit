package repos

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestControlDB_HonoursTestFastDurability opens control.db through the REAL
// constructors and checks they pass the #365 switch through. The store-side
// test rebuilds a control.db DSN by hand, so it would stay green if either
// constructor dropped store.SyncDSNParam; this one would not. Off, control.db
// runs go-sqlite3's WAL default (synchronous=NORMAL); on, synchronous=OFF —
// and WAL either way, since the journal mode decides locking.
//
// It flips a process-wide switch, so it must not run in parallel.
func TestControlDB_HonoursTestFastDurability(t *testing.T) {
	// Restore whatever TestMain set; reading it back through SyncDSNParam
	// avoids a second accessor on store.
	prev := store.SyncDSNParam() != ""
	t.Cleanup(func() { store.SetTestFastDurability(prev) })

	// synchronous: 0 = OFF, 1 = NORMAL.
	check := func(t *testing.T, what string, db *sql.DB, wantSync int) {
		t.Helper()
		var journal string
		var sync int
		require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&journal))
		require.NoError(t, db.QueryRow("PRAGMA synchronous").Scan(&sync))
		require.Equal(t, "wal", journal, "%s must stay in WAL", what)
		require.Equal(t, wantSync, sync, "%s synchronous", what)
	}

	for _, tc := range []struct {
		name     string
		fast     bool
		wantSync int
	}{
		{name: "off (production)", fast: false, wantSync: 1},
		{name: "on", fast: true, wantSync: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.SetTestFastDurability(tc.fast)
			dir := t.TempDir()

			reg, err := OpenRegistryNoSchema(filepath.Join(dir, "registry-control.db"))
			require.NoError(t, err)
			defer reg.Close()
			check(t, "OpenRegistryNoSchema", reg.db, tc.wantSync)

			lr, err := OpenLensRegistry(filepath.Join(dir, "lens-control.db"))
			require.NoError(t, err)
			defer lr.Close()
			check(t, "OpenLensRegistry", lr.db, tc.wantSync)
		})
	}
}
