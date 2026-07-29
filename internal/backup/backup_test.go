package backup

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/litestream"
	_ "github.com/mattn/go-sqlite3"
	"github.com/superfly/ltx"

	"knomit/internal/config"
)

// newTestManager returns a backup Manager replicating to a local file:// URL.
// Litestream's file backend exercises the SAME code path as S3 with no network.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	replica := t.TempDir()
	cfg := config.BackupConfig{
		Enabled:           true,
		URL:               "file://" + replica,
		Instance:          "test",
		SnapshotInterval:  time.Hour,
		SnapshotRetention: time.Hour,
		L0Retention:       time.Minute,
		MonitorInterval:   50 * time.Millisecond,
	}
	m, err := Open(cfg, home)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { m.Close(context.Background()) })
	return m, home
}

// makeDBWithValue creates a small WAL-mode SQLite database holding one row.
func makeDBWithValue(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (?)`, value); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// makeDB creates a small WAL-mode SQLite database with one row.
func makeDB(t *testing.T, path string) { t.Helper(); makeDBWithValue(t, path, "hello") }

// waitInSync blocks until the named DB has replicated at least once, and
// returns the remote TXID it reached.
func waitInSync(t *testing.T, m *Manager, name string) uint64 {
	t.Helper()
	return waitReplicatedPast(t, m, name, 0)
}

// waitReplicatedPast blocks until the named DB has replicated a transaction
// STRICTLY NEWER than after, and local and remote agree on it.
//
// The "strictly newer" part is what makes this usable after a file swap: the
// replica still carries the pre-swap chain, so a bare "RemoteTXID > 0" would
// return instantly and assert nothing about the post-swap content ever having
// been uploaded.
func waitReplicatedPast(t *testing.T, m *Manager, name string, after uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		for _, st := range m.Status(context.Background()) {
			if st.Name == name && st.InSync && st.RemoteTXID > after {
				return st.RemoteTXID
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never replicated past txid %d; status = %+v", name, after, m.Status(context.Background()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertDBValue verifies the database at path carries the expected row.
func assertDBValue(t *testing.T, path, want string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open restored db: %v", err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil {
		t.Fatalf("query restored db: %v", err)
	}
	if v != want {
		t.Errorf("restored value = %q, want %q", v, want)
	}
}

// assertDBHasHello verifies the restored database carries the seeded row.
func assertDBHasHello(t *testing.T, path string) { t.Helper(); assertDBValue(t, path, "hello") }

func TestOpenDisabledReturnsNil(t *testing.T) {
	m, err := Open(config.BackupConfig{Enabled: false}, t.TempDir())
	if err != nil {
		t.Fatalf("Open(disabled): %v", err)
	}
	if m != nil {
		t.Error("Open(disabled) returned a Manager; want nil so callers can no-op")
	}
}

func TestTrackReplicatesAndStatusReports(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath)

	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}

	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := m.Status(ctx)
		if len(st) == 1 && st[0].Name == "core" && st[0].RemoteTXID > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no remote TXID within deadline; status = %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestUntrackRemovesFromStatus(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath)

	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if st := m.Status(context.Background()); len(st) != 0 {
		t.Errorf("Status = %+v, want empty after Untrack", st)
	}
}

// blockingReplicaClient is a litestream.ReplicaClient whose LTXFiles call
// blocks until told to proceed. It stands in for a stalled/slow remote LIST,
// letting a test deterministically observe whether Manager.Status holds its
// lock across that call — without needing real network latency or timing-
// based flakiness.
type blockingReplicaClient struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *blockingReplicaClient) Type() string { return "blocking" }

func (c *blockingReplicaClient) Init(context.Context) error { return nil }

func (c *blockingReplicaClient) LTXFiles(ctx context.Context, level int, seek ltx.TXID, useMetadata bool) (ltx.FileIterator, error) {
	c.once.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return ltx.NewFileInfoSliceIterator(nil), nil
}

func (c *blockingReplicaClient) OpenLTXFile(context.Context, int, ltx.TXID, ltx.TXID, int64, int64) (io.ReadCloser, error) {
	return nil, fmt.Errorf("blockingReplicaClient: OpenLTXFile not implemented")
}

func (c *blockingReplicaClient) WriteLTXFile(context.Context, int, ltx.TXID, ltx.TXID, io.Reader) (*ltx.FileInfo, error) {
	return nil, fmt.Errorf("blockingReplicaClient: WriteLTXFile not implemented")
}

func (c *blockingReplicaClient) DeleteLTXFiles(context.Context, []*ltx.FileInfo) error { return nil }

func (c *blockingReplicaClient) DeleteAll(context.Context) error { return nil }

func (c *blockingReplicaClient) SetLogger(*slog.Logger) {}

// TestStatusDoesNotHoldLockAcrossNetworkCall guards the fix for Status()
// holding m.mu across db.SyncStatus's remote round-trip. It plants a fake
// tracked DB whose replica client blocks in LTXFiles (the call SyncStatus
// drives via Replica.calcPos) until released, starts Status() in the
// background, waits for it to actually be blocked inside that call, and then
// requires Track to complete promptly — Track only needs the manager's
// exclusive lock, which Status must not be holding during the network call.
//
// Against the pre-fix code (RLock held for Status's entire body), Track would
// block on m.mu.Lock() until the blocking client's LTXFiles call returns,
// which this test only allows after Track's deadline — so the pre-fix code
// fails this test with a timeout, not by chance.
func TestStatusDoesNotHoldLockAcrossNetworkCall(t *testing.T) {
	m, home := newTestManager(t)

	started := make(chan struct{})
	release := make(chan struct{})
	client := &blockingReplicaClient{started: started, release: release}

	slowDB := litestream.NewDB(filepath.Join(home, "slow.db"))
	slowDB.Replica = litestream.NewReplicaWithClient(slowDB, client)

	m.mu.Lock()
	m.dbs["slow"] = slowDB
	m.mu.Unlock()

	statusDone := make(chan struct{})
	go func() {
		m.Status(context.Background())
		close(statusDone)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Status never reached the blocking replica client's LTXFiles call")
	}

	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath)

	trackDone := make(chan error, 1)
	go func() { trackDone <- m.Track("core", dbPath) }()

	select {
	case err := <-trackDone:
		close(release)
		<-statusDone
		if err != nil {
			t.Fatalf("Track: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		<-statusDone
		t.Fatal("Track blocked while Status's remote call was in flight: Status is holding the manager lock across a network call")
	}
}
