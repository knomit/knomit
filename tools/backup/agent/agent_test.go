package agent

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	_ "modernc.org/sqlite"

	"knomit/internal/backup/proto"
)

// These tests exercise litestream MECHANISMS that have no protocol surface —
// which store a database is registered with, and what retention does to it.
// They live here rather than in internal/backup because that is where the
// mechanism lives: internal/backup no longer links litestream at all, and
// adding protocol methods purely so a test could reach EnforceSnapshotRetention
// would be inventing production surface to serve a test.
//
// The SQLite driver is modernc.org/sqlite — litestream's own — deliberately.
// Using knomit's cgo build here would recreate, inside this test binary, the
// exact two-builds-one-process condition this entire change exists to remove.

// newTestAgent returns an open Agent replicating to a local file:// URL.
func newTestAgent(t *testing.T) (*Agent, string) {
	t.Helper()
	home := t.TempDir()
	cfg := proto.Config{
		URL:               "file://" + t.TempDir(),
		Instance:          "test",
		SnapshotInterval:  time.Hour,
		SnapshotRetention: time.Hour,
		L0Retention:       time.Minute,
		MonitorInterval:   50 * time.Millisecond,
	}
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := a.Open(context.Background(), cfg); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a, home
}

// makeDB creates a small WAL-mode SQLite database holding one row.
func makeDB(t *testing.T, path, value string) {
	t.Helper()
	db := openDB(t, path)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (?)`, value); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return db
}

// openWriter returns a live connection held open for the rest of the test.
// Holding it open is load-bearing: closing a connection checkpoints and
// truncates the WAL, and litestream then sees no frames it has not shipped, so
// a write through an open-then-close connection never advances the replicated
// TXID.
func openWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := openDB(t, path)
	t.Cleanup(func() { db.Close() })
	return db
}

func track(t *testing.T, a *Agent, name, path, rel string, archived bool) {
	t.Helper()
	if err := a.Track(context.Background(), proto.TrackParams{
		Name: name, Path: path, Rel: rel, Archived: archived,
	}); err != nil {
		t.Fatalf("Track %q: %v", name, err)
	}
}

// waitReplicatedPast blocks until name has replicated a transaction strictly
// newer than after, and returns it.
func waitReplicatedPast(t *testing.T, a *Agent, name string, after uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		sts, err := a.Status(context.Background())
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		for _, st := range sts {
			if st.Name == name && st.InSync && st.RemoteTXID > after {
				return st.RemoteTXID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never replicated past txid %d; status = %+v", name, after, sts)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// countSnapshots reports how many snapshot-level LTX files the tracked database
// currently has on its replica. It reads through the replica CLIENT rather than
// the local cache, because the whole question retention answers is "what is
// still in the object store".
func countSnapshots(t *testing.T, a *Agent, name string) int {
	t.Helper()
	a.mu.RLock()
	tr, ok := a.dbs[name]
	a.mu.RUnlock()
	if !ok {
		t.Fatalf("%q is not tracked", name)
	}
	itr, err := tr.db.Replica.Client.LTXFiles(context.Background(), litestream.SnapshotLevel, 0, false)
	if err != nil {
		t.Fatalf("list snapshots for %q: %v", name, err)
	}
	n := 0
	for itr.Next() {
		n++
	}
	if err := itr.Close(); err != nil {
		t.Fatalf("list snapshots for %q: %v", name, err)
	}
	return n
}

// appendAndSnapshot writes one more row, waits for it to replicate, then forces
// a snapshot — so each call leaves a snapshot at a DISTINCT transaction id.
// Two snapshots at the same position collide on one replica file name and would
// prove nothing about retention.
func appendAndSnapshot(t *testing.T, a *Agent, name string, w *sql.DB, value string) {
	t.Helper()
	a.mu.RLock()
	tr, ok := a.dbs[name]
	a.mu.RUnlock()
	if !ok {
		t.Fatalf("%q is not tracked", name)
	}

	before := waitReplicatedPast(t, a, name, 0)
	if _, err := w.Exec(`INSERT INTO t VALUES (?)`, value); err != nil {
		t.Fatalf("insert into %q: %v", name, err)
	}
	waitReplicatedPast(t, a, name, before)

	if _, err := tr.db.Snapshot(context.Background()); err != nil {
		t.Fatalf("snapshot %q: %v", name, err)
	}
}

// TestArchivedSnapshotsSurviveRetention is the guard on the whole point of the
// archive store: an archived database stops changing, so under the ordinary
// snapshot retention its snapshots would simply age out and "archive" — a
// documented, recoverable state — would become "delete" on a delay.
//
// The test drives litestream's OWN retention enforcement rather than waiting on
// a timer: both databases get three snapshots at distinct transaction ids, the
// retention window on both stores is then set to a NEGATIVE duration (a cutoff
// in the future, so every snapshot is expired), and enforcement runs once on
// each. The live half is the control — if it did not lose snapshots the
// archived half surviving would prove nothing about the fixture.
//
// Three snapshots, not one: litestream always keeps the NEWEST snapshot
// regardless of the window, so a one-snapshot fixture would pass against
// completely broken retention.
func TestArchivedSnapshotsSurviveRetention(t *testing.T) {
	a, home := newTestAgent(t)
	ctx := context.Background()

	livePath := filepath.Join(home, "repos", "live.db")
	archivePath := filepath.Join(home, "repos", "archive", "arc-2.db")
	for _, p := range []string{livePath, archivePath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		makeDB(t, p, "seed")
	}

	track(t, a, "live", livePath, "repos/live.db", false)
	track(t, a, "archive/arc-2", archivePath, "archive/arc-2.db", true)

	liveWriter := openWriter(t, livePath)
	archiveWriter := openWriter(t, archivePath)
	for i := 0; i < 3; i++ {
		appendAndSnapshot(t, a, "live", liveWriter, "live-row")
		appendAndSnapshot(t, a, "archive/arc-2", archiveWriter, "archive-row")
	}

	liveBefore := countSnapshots(t, a, "live")
	archiveBefore := countSnapshots(t, a, "archive/arc-2")
	if liveBefore < 2 || archiveBefore < 2 {
		t.Fatalf("fixture needs >=2 snapshots each; live=%d archive=%d", liveBefore, archiveBefore)
	}

	// A negative retention puts the cutoff in the FUTURE, so every existing
	// snapshot is expired. litestream always keeps the newest one regardless,
	// which is exactly why the assertion below is "lost some" and not "lost all".
	a.store.SnapshotRetention = -time.Hour
	a.archiveStore.SnapshotRetention = -time.Hour

	a.mu.RLock()
	liveDB := a.dbs["live"].db
	archiveDB := a.dbs["archive/arc-2"].db
	a.mu.RUnlock()

	if err := a.store.EnforceSnapshotRetention(ctx, liveDB); err != nil {
		t.Fatalf("enforce retention on the live store: %v", err)
	}
	if err := a.archiveStore.EnforceSnapshotRetention(ctx, archiveDB); err != nil {
		t.Fatalf("enforce retention on the archive store: %v", err)
	}

	if got := countSnapshots(t, a, "live"); got >= liveBefore {
		t.Fatalf("control failed: the LIVE store kept all %d snapshots through an expired window (got %d); "+
			"the archived assertion below would then prove nothing", liveBefore, got)
	}
	if got := countSnapshots(t, a, "archive/arc-2"); got != archiveBefore {
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
	a, home := newTestAgent(t)

	livePath := filepath.Join(home, "repos", "live.db")
	archivePath := filepath.Join(home, "repos", "archive", "arc-3.db")
	for _, p := range []string{livePath, archivePath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		makeDB(t, p, "seed")
	}
	track(t, a, "live", livePath, "repos/live.db", false)
	track(t, a, "archive/arc-3", archivePath, "archive/arc-3.db", true)

	if a.archiveStore.RetentionEnabled {
		t.Error("the archive store deletes expired files; archived snapshots would age out")
	}
	if !a.store.RetentionEnabled {
		t.Error("the live store stopped enforcing retention; live replicas would grow without bound")
	}
	if a.archiveStore.L0Retention != 0 {
		t.Error("the archive store's L0Retention is non-zero; the local LTX sweep runs OUTSIDE the " +
			"RetentionEnabled guard, so archived files would be pruned locally anyway")
	}
	if a.archiveStore.SnapshotRetention != archiveSnapshotRetention {
		t.Errorf("archive SnapshotRetention = %v, want the century-long window", a.archiveStore.SnapshotRetention)
	}

	a.mu.RLock()
	liveDB := a.dbs["live"].db
	archiveDB := a.dbs["archive/arc-3"].db
	a.mu.RUnlock()

	if archiveDB.RetentionEnabled {
		t.Error("archived database was registered with retention ENABLED")
	}
	if !liveDB.RetentionEnabled {
		t.Error("live database was registered with retention disabled")
	}
}

// TestUntrackReturnsADatabaseToTheStoreItCameFrom pins the reason the agent
// REMEMBERS which store a database was registered with rather than deriving it
// from the request. UnregisterDB is scoped to one store: routing an untrack to
// the wrong one is a silent no-op, and the caller is told replication stopped
// while the database keeps replicating.
func TestUntrackReturnsADatabaseToTheStoreItCameFrom(t *testing.T) {
	a, home := newTestAgent(t)
	archivePath := filepath.Join(home, "arc.db")
	makeDB(t, archivePath, "archived")
	track(t, a, "archive/arc-9", archivePath, "archive/arc-9.db", true)

	if got := len(a.archiveStore.DBs()); got != 1 {
		t.Fatalf("archive store holds %d databases, want 1", got)
	}
	if err := a.Untrack("archive/arc-9"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if got := len(a.archiveStore.DBs()); got != 0 {
		t.Fatalf("archive store still holds %d databases after Untrack: the database is still replicating", got)
	}
	for _, db := range a.archiveStore.DBs() {
		if db.IsOpen() {
			t.Fatalf("%s is still open after Untrack", db.Path())
		}
	}
}

// TestTrackRejectsADifferentPathForATrackedName covers the agent's own guard,
// the one that catches client drift. litestream.DB pins a file descriptor via
// os.Open at init and never reopens it, so a track swallowed as "already
// tracked" would leave the caller's file replicated by nobody, with no error
// anywhere.
func TestTrackRejectsADifferentPathForATrackedName(t *testing.T) {
	a, home := newTestAgent(t)
	first := filepath.Join(home, "first.db")
	second := filepath.Join(home, "second.db")
	makeDB(t, first, "first")
	makeDB(t, second, "second")

	track(t, a, "core", first, "repos/core.db", false)
	track(t, a, "core", first, "repos/core.db", false) // same path: a no-op

	err := a.Track(context.Background(), proto.TrackParams{
		Name: "core", Path: second, Rel: "repos/core.db",
	})
	if err == nil {
		t.Fatal("Track silently accepted a different path for a tracked name")
	}
	if got := codeOf(err); got != proto.CodeTrackedElsewhere {
		t.Errorf("code = %q, want %q — the client cannot branch on a message", got, proto.CodeTrackedElsewhere)
	}
}

// TestMethodsBeforeOpenAreRetryable: every method must refuse cleanly before
// open, and with the code that tells the client this is transient rather than
// fatal. A generation is published only after open succeeds, so a not-open
// answer always means "the agent restarted underneath you" — which the client
// retries instead of surfacing as a failed Track.
func TestMethodsBeforeOpenAreRetryable(t *testing.T) {
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	checks := map[string]error{
		"track":             a.Track(ctx, proto.TrackParams{Name: "core", Path: "/tmp/x.db", Rel: "repos/core.db"}),
		"untrack":           a.Untrack("core"),
		"reset_local_state": a.ResetLocalState(ctx, "/tmp/x.db"),
		"delete_replica":    a.DeleteReplica(ctx, "archive/x.db"),
		"preflight":         a.Preflight(ctx, proto.PreflightParams{Name: "core", Path: "/tmp/x.db", Rel: "repos/core.db"}),
	}
	if _, err := a.Status(ctx); err != nil {
		checks["status"] = err
	} else {
		t.Error("Status before open succeeded")
	}
	if _, err := a.Restore(ctx, proto.RestoreParams{Rel: "repos/core.db", Dest: "/tmp/x.db"}); err != nil {
		checks["restore"] = err
	} else {
		t.Error("Restore before open succeeded")
	}

	for name, err := range checks {
		if err == nil {
			t.Errorf("%s before open = nil, want a refusal", name)
			continue
		}
		if got := codeOf(err); got != proto.CodeNotOpen {
			t.Errorf("%s before open: code = %q, want %q", name, got, proto.CodeNotOpen)
		}
	}
}

// TestOpenProbesTheReplicaTarget: an unreachable or unsupported target must
// fail here, at boot, rather than surfacing later as a silent replication
// stall. The scheme set is exactly file/s3/gs, so an undocumented one is a
// refusal by design.
func TestOpenProbesTheReplicaTarget(t *testing.T) {
	a := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := a.Open(context.Background(), proto.Config{URL: "webdav://example.invalid", Instance: "test"})
	if err == nil {
		t.Fatal("Open accepted an unsupported replica scheme")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %v, want it to name the unreachable target", err)
	}
}
