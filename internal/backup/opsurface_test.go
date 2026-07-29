package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStatusReportsTheLastSuccessfulSyncTime pins where the operational
// surface's staleness signal comes from. It is litestream's OWN record of the
// last successful replica sync, carried across the pipe — not a client-side
// "when did we last see it in sync", which would be a property of the polling
// interval rather than of replication.
func TestStatusReportsTheLastSuccessfulSyncTime(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	before := time.Now().Add(-time.Minute).Unix()
	waitInSync(t, m, "core")

	deadline := time.Now().Add(20 * time.Second)
	for {
		var got DBStatus
		for _, st := range m.Status(context.Background()) {
			if st.Name == "core" {
				got = st
			}
		}
		if got.LastSyncUnix > before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("LastSyncUnix = %d, want a recent timestamp (status = %+v)", got.LastSyncUnix, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStatusNeverSyncedReportsZero: zero must survive as zero. Consumers render
// "never synced" by omitting the timestamp entirely, and any client-side
// substitute (time.Now on first sight, say) would report a sync that never
// happened.
func TestStatusNeverSyncedReportsZero(t *testing.T) {
	m, home := newFakeManager(t, fakeNormal)
	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}
	st := m.Status(t.Context())
	if len(st) != 1 {
		t.Fatalf("Status = %+v, want one entry", st)
	}
	if st[0].LastSyncUnix != 0 {
		t.Errorf("LastSyncUnix = %d, want 0 for a database that has never synced", st[0].LastSyncUnix)
	}
}

// TestStatusReportsAPausedDatabase closes a false all-clear. Pause untracks the
// database, so without this it disappears from Status entirely — and an
// operational surface that shows nothing wrong because it shows NOTHING is the
// failure mode this whole surface exists to prevent. A pause that is never
// resumed is a repo that has silently stopped being backed up.
func TestStatusReportsAPausedDatabase(t *testing.T) {
	m, home := newFakeManager(t, fakeNormal)
	dbPath := filepath.Join(home, "core.db")
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}

	resume, err := m.Pause("core")
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	st := m.Status(t.Context())
	if len(st) != 1 || st[0].Name != "core" || !st[0].Paused {
		t.Fatalf("Status while paused = %+v, want one entry for core marked paused", st)
	}
	if st[0].LastError != "" {
		t.Errorf("paused entry carries LastError %q; a deliberate pause is a state, not a failure", st[0].LastError)
	}

	if err := resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	st = m.Status(t.Context())
	if len(st) != 1 || st[0].Paused {
		t.Fatalf("Status after resume = %+v, want core no longer paused", st)
	}
}

// TestRestoreToOverwritesAnExistingDatabase is the one path allowed to replace
// live data. The automatic boot restore fills absences only, which leaves a
// present-but-corrupt database unrecoverable without this.
func TestRestoreToOverwritesAnExistingDatabase(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, dbPath, "good")
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	// Corrupt the local file the way a bad disk would: the file still exists,
	// so restoreIfAbsent would refuse to touch it.
	if err := os.WriteFile(dbPath, []byte("not a database at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.RestoreTo(context.Background(), "core", dbPath, time.Time{}); err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}
	assertDBValue(t, dbPath, "good")
}

// TestRestoreToLeavesTheOriginalWhenTheReplicaHasNothing: a restore that cannot
// find a backup must not have already destroyed the copy the operator still
// has. The failure mode this pins is the obvious implementation — delete the
// destination, then restore into the gap.
func TestRestoreToLeavesTheOriginalWhenTheReplicaHasNothing(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "never-backed-up.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, dbPath, "still here")

	err := m.RestoreTo(context.Background(), "never-backed-up", dbPath, time.Time{})
	if err == nil {
		t.Fatal("RestoreTo against an empty replica succeeded; want a failure naming the missing backup")
	}
	if !strings.Contains(err.Error(), "never-backed-up") {
		t.Errorf("error %q does not name the database", err)
	}
	assertDBValue(t, dbPath, "still here")
}

// TestRestoreToClearsLocalLitestreamState guards the same hazard Pause exists
// for. An overwriting restore replaces the file's identity, so litestream's
// leftover local LTX state describes a database that no longer exists — and
// continuing that chain uploads deltas against the wrong pages, silently.
func TestRestoreToClearsLocalLitestreamState(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, dbPath, "good")
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	ltxDir := filepath.Join(filepath.Dir(dbPath), ".core.db-litestream", "ltx")
	if _, err := os.Stat(ltxDir); err != nil {
		t.Skipf("litestream local state is not at %s in this version: %v", ltxDir, err)
	}

	if err := m.RestoreTo(context.Background(), "core", dbPath, time.Time{}); err != nil {
		t.Fatalf("RestoreTo: %v", err)
	}
	if _, err := os.Stat(ltxDir); !os.IsNotExist(err) {
		t.Errorf("local LTX state at %s survived an overwriting restore (%v); the next sync would "+
			"continue a chain describing the replaced file", ltxDir, err)
	}
}

// TestRestoreToRefusesADatabaseThisAgentIsReplicating: restoring under a live
// replica is the two-writers case in miniature. knomit restore is documented as
// operating on a STOPPED server, and this is the enforcement of that.
func TestRestoreToRefusesADatabaseThisAgentIsReplicating(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, dbPath, "good")
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")

	err := m.RestoreTo(context.Background(), "core", dbPath, time.Time{})
	if err == nil {
		t.Fatal("RestoreTo overwrote a database that is actively replicating")
	}
	assertDBValue(t, dbPath, "good")
}

func TestRestoreToOnADisabledManagerSaysSo(t *testing.T) {
	var m *Manager
	err := m.RestoreTo(context.Background(), "core", "/tmp/x.db", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("RestoreTo on a nil Manager = %v, want a 'backup is not enabled' error", err)
	}
}
