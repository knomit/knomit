package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
)

// countSnapshots reports how many snapshot-level LTX files the tracked database
// currently has on its replica. It reads through the replica CLIENT rather than
// the local cache, because the whole question retention answers is "what is
// still in the object store".
func countSnapshots(t *testing.T, m *Manager, name string) int {
	t.Helper()
	m.mu.RLock()
	db := m.dbs[name]
	m.mu.RUnlock()
	if db == nil {
		t.Fatalf("%q is not tracked", name)
	}
	itr, err := db.Replica.Client.LTXFiles(context.Background(), litestream.SnapshotLevel, 0, false)
	if err != nil {
		t.Fatalf("list snapshots for %q: %v", name, err)
	}
	defer itr.Close()
	n := 0
	for itr.Next() {
		n++
	}
	if err := itr.Close(); err != nil {
		t.Fatalf("list snapshots for %q: %v", name, err)
	}
	return n
}

// openWriter returns a live connection to a tracked database, held open for the
// rest of the test.
//
// Holding it open is load-bearing, not tidiness: knomit's mattn connection is
// the only non-litestream one, so closing it checkpoints the WAL and TRUNCATES
// it, and litestream — which polls the WAL for frames it has not yet shipped —
// then observes no change at all. A write made through an open-then-close
// connection never advances the replicated TXID, so a test built that way waits
// forever for a transaction litestream will never see.
func openWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	sqldb, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { sqldb.Close() })
	return sqldb
}

// appendAndSnapshot writes one more row, waits for it to replicate, then forces
// a snapshot — so each call leaves a snapshot at a DISTINCT transaction id.
// Two snapshots at the same position collide on one replica file name and would
// prove nothing about retention.
func appendAndSnapshot(t *testing.T, m *Manager, name string, w *sql.DB, value string) {
	t.Helper()
	m.mu.RLock()
	db := m.dbs[name]
	m.mu.RUnlock()
	if db == nil {
		t.Fatalf("%q is not tracked", name)
	}

	before := waitInSync(t, m, name)
	if _, err := w.Exec(`INSERT INTO t VALUES (?)`, value); err != nil {
		t.Fatalf("insert into %q: %v", name, err)
	}
	waitReplicatedPast(t, m, name, before)

	if _, err := db.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot %q: %v", name, err)
	}
}

func TestArchivedDBReplicatesUnderArchivePrefix(t *testing.T) {
	m, home := newTestManager(t)
	archivePath := filepath.Join(home, "repos", "archive", "arc-1.db")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, archivePath, "archived-content")

	if err := m.TrackArchived("arc-1", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}
	waitInSync(t, m, "archive/arc-1")

	// Wipe locally, then restore straight from the archive prefix.
	if err := m.Untrack("archive/arc-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archivePath); err != nil {
		t.Fatal(err)
	}
	restored, err := m.restoreIfAbsent(context.Background(), "archive/arc-1.db", archivePath)
	if err != nil {
		t.Fatalf("restore archived: %v", err)
	}
	if !restored {
		t.Fatal("archived DB was not restored")
	}
	assertDBValue(t, archivePath, "archived-content")
}

// TestArchivedSnapshotsSurviveRetention is the guard on the whole point of the
// archive prefix: an archived database stops changing, so under the ordinary
// snapshot retention its snapshots would simply age out and "archive" — a
// documented, recoverable state — would become "delete" on a delay.
//
// The test drives litestream's OWN retention enforcement rather than waiting on
// a timer: both databases get three snapshots at distinct transaction ids, the
// retention window on both stores is then set to a NEGATIVE duration (a cutoff
// in the future, so every snapshot is expired), and enforcement runs once on
// each. The live half is the control — if it did not lose snapshots the archived
// half surviving would prove nothing about the fixture.
func TestArchivedSnapshotsSurviveRetention(t *testing.T) {
	m, home := newTestManager(t)
	ctx := context.Background()

	livePath := filepath.Join(home, "repos", "live.db")
	archivePath := filepath.Join(home, "repos", "archive", "arc-2.db")
	for _, p := range []string{livePath, archivePath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		makeDBWithValue(t, p, "seed")
	}

	if err := m.Track("live", livePath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.TrackArchived("arc-2", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}

	liveWriter := openWriter(t, livePath)
	archiveWriter := openWriter(t, archivePath)
	for i := 0; i < 3; i++ {
		appendAndSnapshot(t, m, "live", liveWriter, "live-row")
		appendAndSnapshot(t, m, "archive/arc-2", archiveWriter, "archive-row")
	}

	liveBefore := countSnapshots(t, m, "live")
	archiveBefore := countSnapshots(t, m, "archive/arc-2")
	if liveBefore < 2 || archiveBefore < 2 {
		t.Fatalf("fixture needs >=2 snapshots each; live=%d archive=%d", liveBefore, archiveBefore)
	}

	// A negative retention puts the cutoff in the FUTURE, so every existing
	// snapshot is expired. litestream always keeps the newest one regardless,
	// which is exactly why the assertion below is "lost some" and not "lost all".
	m.store.SnapshotRetention = -time.Hour
	m.archiveStore.SnapshotRetention = -time.Hour

	m.mu.RLock()
	liveDB := m.dbs["live"]
	archiveDB := m.dbs["archive/arc-2"]
	m.mu.RUnlock()

	if err := m.store.EnforceSnapshotRetention(ctx, liveDB); err != nil {
		t.Fatalf("enforce retention on the live store: %v", err)
	}
	if err := m.archiveStore.EnforceSnapshotRetention(ctx, archiveDB); err != nil {
		t.Fatalf("enforce retention on the archive store: %v", err)
	}

	if got := countSnapshots(t, m, "live"); got >= liveBefore {
		t.Fatalf("control failed: the LIVE store kept all %d snapshots through an expired window (got %d); "+
			"the archived assertion below would then prove nothing", liveBefore, got)
	}
	if got := countSnapshots(t, m, "archive/arc-2"); got != archiveBefore {
		t.Errorf("archived snapshots = %d, want %d: retention deleted archived snapshots — "+
			"archive has silently become delete-on-a-delay", got, archiveBefore)
	}
}

// TestArchiveStoreHasRetentionDisabled pins the mechanism, not just the effect.
// litestream v0.5.15 has no per-replica retention knob: Store.RegisterDB
// OVERWRITES DB.RetentionEnabled from the store's own value just before
// DB.Open() copies it into the compactor, so the only place the setting can be
// made to stick is the store a database is registered with. If a later refactor
// routes archived databases back through the live store, this fails immediately
// rather than at the next retention sweep.
func TestArchiveStoreHasRetentionDisabled(t *testing.T) {
	m, home := newTestManager(t)

	livePath := filepath.Join(home, "repos", "live.db")
	archivePath := filepath.Join(home, "repos", "archive", "arc-3.db")
	for _, p := range []string{livePath, archivePath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		makeDBWithValue(t, p, "seed")
	}
	if err := m.Track("live", livePath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.TrackArchived("arc-3", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}

	if m.archiveStore.RetentionEnabled {
		t.Error("the archive store deletes expired files; archived snapshots would age out")
	}
	if !m.store.RetentionEnabled {
		t.Error("the live store stopped enforcing retention; live replicas would grow without bound")
	}

	m.mu.RLock()
	liveDB := m.dbs["live"]
	archiveDB := m.dbs["archive/arc-3"]
	m.mu.RUnlock()

	if archiveDB.RetentionEnabled {
		t.Error("archived database was registered with retention ENABLED")
	}
	if !liveDB.RetentionEnabled {
		t.Error("live database was registered with retention disabled")
	}
}

// TestArchiveHandoverLetsAReclaimedNameReplicate is the end-to-end proof of the
// archive ordering, run against real litestream rather than a double — because
// the hazard is at the INODE level and no path-comparing double can show it.
//
// The sequence is exactly repos.Archive's: untrack the live name, move the file,
// track it under the archive id. A NEW database then takes the freed path — the
// SAME path string, a different inode. litestream.DB pins its descriptor with a
// single os.Open at init, so had the live entry stayed tracked, the reclaiming
// database would replicate nothing at all: the pinned descriptor still refers to
// the moved inode, the name is "already tracked" so Track is a no-op, and Status
// keeps reporting in sync. The assertion is that BOTH prefixes decode to their
// own content.
func TestArchiveHandoverLetsAReclaimedNameReplicate(t *testing.T) {
	m, home := newTestManager(t)
	ctx := context.Background()

	livePath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(livePath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDBWithValue(t, livePath, "first-tenant")
	if err := m.Track("core", livePath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	first := waitInSync(t, m, "core")

	// Archive, in the order repos.Archive uses.
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	archivePath := filepath.Join(home, "repos", "archive", "arc-4.db")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(livePath, archivePath); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(livePath+suffix, archivePath+suffix)
	}
	if err := m.TrackArchived("arc-4", archivePath); err != nil {
		t.Fatalf("TrackArchived: %v", err)
	}
	waitInSync(t, m, "archive/arc-4")

	// A new repo claims the freed name — same path, new inode.
	makeDBWithValue(t, livePath, "second-tenant")
	if err := m.Track("core", livePath); err != nil {
		t.Fatalf("Track(reclaimed): %v", err)
	}
	// Strictly past the first tenant's position: proves the NEW inode's content
	// actually reached the live prefix, not merely that the name looks in sync.
	waitReplicatedPast(t, m, "core", first)

	if err := m.Untrack("core"); err != nil {
		t.Fatal(err)
	}
	if err := m.Untrack("archive/arc-4"); err != nil {
		t.Fatal(err)
	}

	liveOut := filepath.Join(t.TempDir(), "live-restored.db")
	if _, err := m.restoreIfAbsent(ctx, m.relFor("core"), liveOut); err != nil {
		t.Fatalf("restore live prefix: %v", err)
	}
	assertDBValue(t, liveOut, "second-tenant")

	archiveOut := filepath.Join(t.TempDir(), "archive-restored.db")
	if _, err := m.restoreIfAbsent(ctx, m.relFor("archive/arc-4"), archiveOut); err != nil {
		t.Fatalf("restore archive prefix: %v", err)
	}
	assertDBValue(t, archiveOut, "first-tenant")
}

// TestTrackRefusesADifferentPathForATrackedName closes the silent half of the
// archive bug — the half a path comparison CAN see.
//
// litestream.DB pins a file descriptor via os.Open at init and never reopens it,
// so a Track swallowed as "already tracked" leaves the caller's file replicated
// by nobody, with no error anywhere. Note what this check does and does not
// cover: a reclaimed repo name resolves to the same path string, so only
// Archive's untrack can close that case (see the test above). This one catches
// the rest — a name re-pointed at a genuinely different file, which is always a
// caller bug rather than a no-op.
func TestTrackRefusesADifferentPathForATrackedName(t *testing.T) {
	m, home := newTestManager(t)

	first := filepath.Join(home, "first.db")
	second := filepath.Join(home, "second.db")
	makeDBWithValue(t, first, "first")
	makeDBWithValue(t, second, "second")

	if err := m.Track("core", first); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.Track("core", first); err != nil {
		t.Fatalf("re-Track of the same path must stay a no-op: %v", err)
	}
	err := m.Track("core", second)
	if err == nil {
		t.Fatal("Track silently ignored a different path for an already-tracked name; " +
			"the new database would replicate nothing, with no error anywhere")
	}
	if got := m.Status(context.Background()); len(got) != 1 || got[0].Name != "core" {
		t.Fatalf("the rejected Track disturbed the tracked set: %+v", got)
	}
}
