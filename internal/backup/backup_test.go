package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/config"
)

// newTestManager returns a backup Manager driving a REAL knomit-backup agent,
// replicating to a local file:// URL. Litestream's file backend exercises the
// same code path as S3 with no network.
func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	replica := t.TempDir()
	cfg := config.BackupConfig{
		Enabled:           true,
		URL:               "file://" + replica,
		Instance:          "test",
		AgentPath:         agentBin,
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

// makeDBWithValue creates a small WAL-mode SQLite database holding one row,
// through knomit's own cgo driver — the one whose locks did not conflict with
// litestream's while they shared a process.
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
	deadline := time.Now().Add(20 * time.Second)
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

func TestTrackReplicatesAndStatusReports(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath)

	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}

	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
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

// TestKnomitCloseNoLongerDestroysTheTrackedWAL is the inverse of the
// demonstration that forced litestream out of knomit's process, and the reason
// this whole change exists.
//
// In-process, this sequence reproduced 3 times out of 3:
//
//	NOT COORDINATED: knomit's close DELETED the -wal while litestream held a
//	                 read lock and PERSIST_WAL
//	NOT COORDINATED: knomit's close removed the -shm while litestream had it mapped
//
// SQLite only deletes a WAL on close after taking an EXCLUSIVE lock, which a
// live reader's shared lock must make impossible. It succeeded anyway because
// POSIX advisory record locks do not conflict between descriptors held by the
// SAME process, and SQLite's compensating per-process inode table is private to
// one SQLite BUILD.
//
// With the agent in its own process the kernel arbitrates normally, so knomit's
// close cannot take the exclusive lock and both sidecars survive. The final
// assertion is the consequence that matters: replication continues afterwards
// and the data is restorable — the sidecars surviving is the mechanism, not the
// point.
func TestKnomitCloseNoLongerDestroysTheTrackedWAL(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	before := waitInSync(t, m, "core")

	// knomit's own connection: opened, written through, and closed while the
	// agent is tracking the same file. The close is the dangerous half.
	sqldb, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sqldb.Exec(`INSERT INTO t VALUES ('written-by-knomit')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := sqldb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err != nil {
			t.Fatalf("knomit's close destroyed %s out from under the replication agent (stat: %v); "+
				"the two SQLite builds are not being arbitrated by the kernel", dbPath+suffix, err)
		}
	}

	// And the consequence: the write still reaches the replica, and the final
	// sync on untrack succeeds rather than failing with "open <db>-wal: no such
	// file or directory".
	waitReplicatedPast(t, m, "core", before)
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("final sync after knomit's close: %v", err)
	}
	wipeLocal(t, dbPath)
	restored, err := m.restoreIfAbsent(context.Background(), m.relFor("core"), dbPath)
	if err != nil || !restored {
		t.Fatalf("restore after knomit's close = (%v, %v), want a restored database", restored, err)
	}
}

// TestAgentCrashIsRecoveredAndTrackedStateReEstablished is the safety-critical
// one. The agent holds its tracked set in MEMORY, so a crash that is merely
// survived — process restarted, nothing re-registered — leaves every repo
// silently unreplicated while Status happily reports names that nothing is
// backing up. A repo that stops replicating with nobody noticing is exactly the
// failure this feature exists to prevent.
//
// The agent is SIGKILLed (no chance to clean up, the worst case), and the
// assertion is not "a process exists again" but "the database replicated a
// transaction written AFTER the crash" — the only evidence that the tracked
// state actually came back.
func TestAgentCrashIsRecoveredAndTrackedStateReEstablished(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	beforeCrash := waitInSync(t, m, "core")

	oldPID := m.cl.currentPID()
	killAgent(t, m)

	// A live writer, held open: closing a connection checkpoints and truncates
	// the WAL, and litestream then sees no frames it has not already shipped —
	// so a write made through an open-then-close connection can never advance
	// the replicated TXID.
	writer := openWriter(t, dbPath)
	if _, err := writer.Exec(`INSERT INTO t VALUES ('after the crash')`); err != nil {
		t.Fatalf("insert after crash: %v", err)
	}

	afterCrash := waitReplicatedPast(t, m, "core", beforeCrash)
	if afterCrash <= beforeCrash {
		t.Fatalf("txid %d did not advance past %d after the agent crash", afterCrash, beforeCrash)
	}
	if newPID := m.cl.currentPID(); newPID == 0 || newPID == oldPID {
		t.Fatalf("agent pid = %d, want a new process (was %d)", newPID, oldPID)
	}
}

// killAgent SIGKILLs the live agent — no chance to clean up, the worst case a
// supervisor has to survive.
func killAgent(t *testing.T, m *Manager) {
	t.Helper()
	pid := m.cl.currentPID()
	if pid == 0 {
		t.Fatal("no agent process to kill")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill agent %d: %v", pid, err)
	}
}

// waitForNewAgent blocks until the supervisor has published a generation other
// than oldPID.
func waitForNewAgent(t *testing.T, m *Manager, oldPID int) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if pid := m.cl.currentPID(); pid != 0 && pid != oldPID {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no new agent replaced pid %d", oldPID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// openWriter returns a live connection to a tracked database, held open for the
// rest of the test.
//
// Holding it open is load-bearing, not tidiness: closing it checkpoints the WAL
// and TRUNCATES it, and litestream — which polls the WAL for frames it has not
// yet shipped — then observes no change at all.
func openWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	sqldb, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { sqldb.Close() })
	return sqldb
}

// TestTrackRacingCloseLeavesNoAgentBehind is the descendant of the in-process
// "Track racing Close must not leak an open database" guard. The hazard has
// changed shape but not disappeared: Close and the supervisor both decide
// whether a child process should exist, and a Track landing in that window
// could resurrect one. An orphaned agent replicating to the same prefix as its
// successor is the two-writers case knomit deliberately never auto-repairs
// (litestream would resolve it by RESETTING the replica, discarding history), so
// it must never be created in the first place.
func TestTrackRacingCloseLeavesNoAgentBehind(t *testing.T) {
	for i := 0; i < 15; i++ {
		m, home := newTestManager(t)
		dbPath := filepath.Join(home, fmt.Sprintf("core%d.db", i))
		makeDB(t, dbPath)
		pid := m.cl.currentPID()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = m.Track("core", dbPath)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = m.Close(context.Background())
		}()
		close(start)
		wg.Wait()

		if p := m.cl.currentPID(); p != 0 {
			t.Fatalf("iteration %d: an agent (pid %d) is still current after Close", i, p)
		}
		if alive(pid) {
			t.Fatalf("iteration %d: agent pid %d outlived Close", i, pid)
		}
	}
}

// alive reports whether pid still names a live process. Signal 0 performs the
// permission and existence checks without delivering anything.
func alive(pid int) bool {
	if pid == 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// TestCloseStopsTheAgent pins the shutdown contract in isolation: after Close
// the child is gone. Never leaving an orphan is not tidiness — an agent that
// outlives knomit keeps writing to the replica prefix the next knomit will
// claim.
func TestCloseStopsTheAgent(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "core.db")
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	pid := m.cl.currentPID()
	if pid == 0 {
		t.Fatal("no agent process")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("agent pid %d survived Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}
