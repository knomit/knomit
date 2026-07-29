package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/repos"
)

// replaceDBFile replaces the database at path with a fresh one holding value,
// the way repos.Manager.SwapStore replaces a repo's database:
//
//   - the sidecar -wal/-shm are gone by the time the file is replaced, because
//     knomit's own SQLite handle is the LAST one to close (backup is paused
//     first) and a close without PERSIST_WAL checkpoints and deletes the WAL;
//   - the file itself is TRUNCATED AND REWRITTEN IN PLACE (copyFile → os.Create),
//     not renamed, so the inode is unchanged and any handle still open on it
//     silently observes the new bytes. That is precisely why "keep replicating
//     across the swap" corrupts rather than fails loudly.
func replaceDBFile(t *testing.T, path, value string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "swapped.db")
	makeDBWithValue(t, src, value)

	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", sidecar, err)
		}
	}

	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open swapped db: %v", err)
	}
	defer in.Close()
	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy over %s: %v", path, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// wipeLocal removes a database and every piece of local state derived from it,
// modelling the fresh volume a restore actually targets.
func wipeLocal(t *testing.T, path string) {
	t.Helper()
	dir, file := filepath.Split(path)
	for _, p := range []string{path, path + "-wal", path + "-shm", filepath.Join(dir, "."+file+"-litestream")} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatalf("remove %s: %v", p, err)
		}
	}
}

// TestPauseResumeSurvivesFileSwap is the whole point of Pause. It tracks a
// database, replaces the FILE underneath (as SwapStore does), resumes, and then
// wipes the machine and restores from the replica: the restore must reproduce
// the POST-swap database.
//
// Continuing the old LTX chain across the swap does not fail here — it produces
// a replica that decodes into pre-swap (or mixed, i.e. corrupt) content, which
// is why the assertion is on the restored VALUE and not on an error being nil.
func TestPauseResumeSurvivesFileSwap(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	preSwap := waitInSync(t, m, "core")

	resume, err := m.Pause("core")
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	replaceDBFile(t, dbPath, "swapped")

	if err := resume(); err != nil {
		t.Fatalf("resume after swap: %v", err)
	}
	// Strictly past the pre-swap position: the post-swap content must actually
	// have been uploaded, not merely "some replication happened once".
	waitReplicatedPast(t, m, "core", preSwap)

	// Wipe and restore: the replica must reproduce the POST-swap database.
	if err := m.Untrack("core"); err != nil {
		t.Fatal(err)
	}
	wipeLocal(t, dbPath)

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.Restored) != 1 {
		t.Fatalf("Restored = %v (failed: %v), want [core]", rep.Restored, rep.Failed)
	}
	assertDBValue(t, dbPath, "swapped")
}

// TestResumeKeepsTXIDMonotonicForPreflight guards the OTHER half of the
// contract. Forcing a fresh snapshot must not restart the transaction chain at
// 1: Preflight refuses to boot whenever the replica is ahead of the local
// database, so a resume that resets the local position without re-anchoring to
// the replica plants a boot failure that only detonates on the next restart.
func TestResumeKeepsTXIDMonotonicForPreflight(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	preSwap := waitInSync(t, m, "core")

	resume, err := m.Pause("core")
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	replaceDBFile(t, dbPath, "swapped")
	if err := resume(); err != nil {
		t.Fatalf("resume after swap: %v", err)
	}
	postSwap := waitReplicatedPast(t, m, "core", preSwap)
	if postSwap <= preSwap {
		t.Fatalf("post-swap txid %d did not advance past %d", postSwap, preSwap)
	}

	if err := m.Preflight(context.Background(), "core", dbPath); err != nil {
		t.Fatalf("Preflight after swap+resume = %v, want nil: the swap must leave the local database at or ahead of its replica", err)
	}
}

// TestPauseIsNoOpForUntrackedDB keeps callers free of nil/tracked checks: a
// paused-but-never-tracked database yields a working no-op resume.
func TestPauseIsNoOpForUntrackedDB(t *testing.T) {
	m, _ := newTestManager(t)
	resume, err := m.Pause("nonexistent")
	if err != nil {
		t.Fatalf("Pause(untracked) = %v, want nil", err)
	}
	if err := resume(); err != nil {
		t.Errorf("resume() = %v, want nil", err)
	}
}

// TestPauseAfterCloseIsNoOp: a swap racing shutdown must not fail with
// "manager is closed" — every replica is already stopped.
func TestPauseAfterCloseIsNoOp(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	resume, err := m.Pause("core")
	if err != nil {
		t.Fatalf("Pause after Close = %v, want nil", err)
	}
	if err := resume(); err != nil {
		t.Errorf("resume() after Close = %v, want nil", err)
	}
}

// TestPauseOnNilManagerIsNoOp: backup disabled is a nil *Manager everywhere.
func TestPauseOnNilManagerIsNoOp(t *testing.T) {
	var m *Manager
	resume, err := m.Pause("core")
	if err != nil {
		t.Fatalf("(*Manager)(nil).Pause = %v, want nil", err)
	}
	if err := resume(); err != nil {
		t.Errorf("resume() = %v, want nil", err)
	}
}
