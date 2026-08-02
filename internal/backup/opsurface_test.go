package backup

import (
	"context"
	"path/filepath"
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

// TestUntrackClearsAPausedMark: Pause is temporary, Untrack is permanent, and
// the combination is reachable — repos.SwapStore resumes from a defer, a failed
// resume deliberately keeps the mark, and archiving or purging that repo then
// untracks it. Without this the surface reports a paused database that no longer
// exists, forever. A permanent phantom is the wrong kind of lie for a surface
// whose whole premise is that it never shows a false state.
func TestUntrackClearsAPausedMark(t *testing.T) {
	m, home := newFakeManager(t, fakeNormal)
	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if _, err := m.Pause("core"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if st := m.Status(t.Context()); len(st) != 0 {
		t.Fatalf("Status after untracking a paused database = %+v, want empty", st)
	}
}

// TestCloseClearsPausedMarks: after Close nothing is replicating and no resume
// will ever run, so a paused mark can only mislead. The tracked entries are
// still reported, but each carries an error saying the manager is closed — a
// paused entry carries no error at all, and would read as "paused, resuming
// shortly" forever.
func TestCloseClearsPausedMarks(t *testing.T) {
	m, home := newFakeManager(t, fakeNormal)
	if err := m.Track("core", filepath.Join(home, "core.db")); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if _, err := m.Pause("core"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, st := range m.Status(context.Background()) {
		if st.Paused {
			t.Errorf("Status after Close still reports %q as paused; nothing will ever resume it", st.Name)
		}
	}
}
