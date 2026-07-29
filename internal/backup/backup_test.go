package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

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

// makeDB creates a small WAL-mode SQLite database with one row.
func makeDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('hello');`); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

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
