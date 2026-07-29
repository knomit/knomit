package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"knomit/internal/repos"
)

func TestRestoreControlNoSnapshotIsNotAnError(t *testing.T) {
	m, home := newTestManager(t)
	if err := m.RestoreControl(context.Background()); err != nil {
		t.Fatalf("RestoreControl with empty bucket = %v, want nil (first boot is normal)", err)
	}
	if _, err := os.Stat(filepath.Join(home, "control.db")); !os.IsNotExist(err) {
		t.Error("RestoreControl created control.db from nothing; want it left absent for a fresh start")
	}
}

func TestRestoreReposSeparatesNoSnapshotFromFailure(t *testing.T) {
	m, _ := newTestManager(t)
	intended := []repos.RepoRecord{
		{Name: "core", State: repos.RepoActive},
		{Name: "notes", State: repos.RepoActive},
	}
	rep, err := m.RestoreRepos(context.Background(), intended)
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.NoSnapshot) != 2 {
		t.Errorf("NoSnapshot = %v, want both repos (empty bucket)", rep.NoSnapshot)
	}
	if len(rep.Failed) != 0 {
		t.Errorf("Failed = %v, want empty — a missing backup is not a failure", rep.Failed)
	}
	if len(rep.Restored) != 0 {
		t.Errorf("Restored = %v, want empty", rep.Restored)
	}
}

func TestRestoreDoesNotOverwriteExistingFile(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("PRECIOUS"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.Restored) != 0 {
		t.Errorf("Restored = %v, want empty — restore must only fill absences", rep.Restored)
	}
	got, _ := os.ReadFile(dbPath)
	if string(got) != "PRECIOUS" {
		t.Fatalf("existing file was overwritten: %q", got)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")

	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.Restored) != 1 || rep.Restored[0] != "core" {
		t.Fatalf("Restored = %v, want [core]", rep.Restored)
	}
	assertDBHasHello(t, dbPath)
}

// TestRestoreReposReportsGenuineFailureSeparately exercises the `case err !=
// nil` branch of RestoreRepos — the one a later task uses to refuse the boot.
// It must never be confused with NoSnapshot: a real error (here, a broken
// filesystem path) has to land in Failed, not be silently treated as "first
// boot is normal."
func TestRestoreReposReportsGenuineFailureSeparately(t *testing.T) {
	m, home := newTestManager(t)

	// Make repos/ a regular file so MkdirAll(filepath.Dir(dst)) for
	// <home>/repos/core.db fails with a real, non-sentinel filesystem error —
	// nothing litestream-related, nothing that isNoSnapshot recognizes.
	reposPath := filepath.Join(home, "repos")
	if err := os.WriteFile(reposPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one entry", rep.Failed)
	}
	if _, ok := rep.Failed["core"]; !ok {
		t.Errorf("Failed = %v, want key %q present", rep.Failed, "core")
	}
	if len(rep.NoSnapshot) != 0 {
		t.Errorf("NoSnapshot = %v, want empty — a real failure must not be misrouted as no-snapshot", rep.NoSnapshot)
	}
	if len(rep.Restored) != 0 {
		t.Errorf("Restored = %v, want empty", rep.Restored)
	}
}

// TestRestoreClearsOrphanedSidecars covers the silent-corruption path that
// restoreIfAbsent's absence check alone does not close.
//
// restoreIfAbsent keys only off the .db file and writes only the .db file. A
// leftover -wal from a previous incarnation of that database (partial manual
// deletion, an interrupted wipe, or a crash inside Create's own
// db/-wal/-shm removal sequence) therefore survives the restore — and SQLite
// will happily REPLAY it onto the restored file on first open, because a WAL
// header carries no database identity and cannot be recognised as foreign.
// The result is silent corruption of data that was just restored correctly.
//
// The sidecars are worthless once the .db is gone (a WAL is a page delta, not a
// standalone database), so removing them loses nothing recoverable.
func TestRestoreClearsOrphanedSidecars(t *testing.T) {
	m, home := newTestManager(t)

	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	wipeLocal(t, dbPath)

	// The orphans: -wal/-shm (WAL mode) and -journal (rollback mode), none of
	// them with a .db beside them. Contents are junk on purpose — the point is
	// that they must never reach SQLite at all.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.WriteFile(dbPath+suffix, []byte("stale frames from a previous incarnation"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.Restored) != 1 {
		t.Fatalf("Restored = %v (failed: %v), want [core]", rep.Restored, rep.Failed)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + suffix); !os.IsNotExist(err) {
			t.Errorf("%s survived the restore; SQLite would replay it onto the restored database", dbPath+suffix)
		}
	}
	assertDBHasHello(t, dbPath)
}

// TestRestoreFailsWhenOrphanedSidecarCannotBeCleared pins the other half of the
// rule: if the orphan cannot be removed we must NOT restore anyway. Restoring
// into a path that still holds foreign WAL frames is the corruption this guards
// against, so the repo has to land in Failed (which refuses the boot) instead.
func TestRestoreFailsWhenOrphanedSidecarCannotBeCleared(t *testing.T) {
	m, home := newTestManager(t)

	dbPath := filepath.Join(home, "repos", "core.db")
	// A NON-EMPTY DIRECTORY at the -wal path: os.Remove refuses it (ENOTEMPTY),
	// which is the only portable way to make the cleanup fail on purpose.
	if err := os.MkdirAll(filepath.Join(dbPath+"-wal", "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if _, ok := rep.Failed["core"]; !ok {
		t.Fatalf("Failed = %v, want core present — an unclearable orphan must refuse, not restore", rep.Failed)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Error("restored the database anyway, into a path still holding foreign WAL frames")
	}
}

// TestPreflightDetectsDivergedReplica covers the comparison direction in
// Preflight directly: RemoteTXID > LocalTXID must trip ErrDiverged. A `<`
// instead of `>` here would either never fire or always fire, and no other
// test in this package would notice.
//
// The divergence must be simulated with the litestream shadow directory
// INTACT — a local database that still claims a position in the chain, just an
// older one than the replica holds. That is the real "two writers, or an old
// volume reattached" shape, and it is the only shape Preflight can distinguish:
// a local file with NO shadow directory is indistinguishable from a freshly
// restored one (see TestPreflightAllowsRestoredDatabaseWithNoLocalState).
//
// Approach: replicate one database under "core" and stop, leaving its shadow
// directory at transaction 1. Then let a SECOND writer replicate to the same
// name from its own file: it re-anchors and pushes the replica to transaction 2.
// The first database is now a stale volume with intact local state — the two-
// writers case verbatim.
func TestPreflightDetectsDivergedReplica(t *testing.T) {
	m, home := newTestManager(t)

	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	first := waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	otherWriter := filepath.Join(home, "other-core.db")
	makeDBWithValue(t, otherWriter, "other writer")
	if err := m.Track("core", otherWriter); err != nil {
		t.Fatalf("Track(second writer): %v", err)
	}
	waitReplicatedPast(t, m, "core", first)
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack(second writer): %v", err)
	}

	err := m.Preflight(context.Background(), "core", dbPath)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("Preflight = %v, want ErrDiverged", err)
	}
}

// TestPreflightAllowsRestoredDatabaseWithNoLocalState is the boot that the
// whole backup feature depends on: restore a database from the replica, then
// preflight it exactly as startup does. restoreIfAbsent writes ONLY the .db
// file, so the restored database has no litestream shadow directory and reports
// local TXID 0 against a replica holding real history. Treating that as
// divergence would refuse every single boot after a restore.
func TestPreflightAllowsRestoredDatabaseWithNoLocalState(t *testing.T) {
	m, home := newTestManager(t)

	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}
	wipeLocal(t, dbPath) // the fresh volume a restore actually lands on

	rep, err := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if err != nil {
		t.Fatalf("RestoreRepos: %v", err)
	}
	if len(rep.Restored) != 1 {
		t.Fatalf("Restored = %v (failed: %v), want [core]", rep.Restored, rep.Failed)
	}

	if err := m.Preflight(context.Background(), "core", dbPath); err != nil {
		t.Fatalf("Preflight after restore = %v, want nil: a restored database has no local litestream state, and refusing it would make every post-restore boot fail", err)
	}
}

// TestPreflightAllowsResetLocalStateWindow covers the same shape from the other
// direction: Pause's ResetLocalState leaves the database with a live replica and
// no local LTX state until litestream's asynchronous re-anchor lands. A crash in
// that window must not poison the next boot.
func TestPreflightAllowsResetLocalStateWindow(t *testing.T) {
	m, home := newTestManager(t)

	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)
	if err := m.Track("core", dbPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	// Exactly what resume() does before re-registering, and then nothing else:
	// the process "crashed" before litestream could re-anchor.
	dir, file := filepath.Split(dbPath)
	if err := os.RemoveAll(filepath.Join(dir, "."+file+"-litestream", "ltx")); err != nil {
		t.Fatal(err)
	}

	if err := m.Preflight(context.Background(), "core", dbPath); err != nil {
		t.Fatalf("Preflight in the reset window = %v, want nil", err)
	}
}

// TestPreflightAllowsLocalFileWithNoReplicaYet covers "first run with an
// existing DB": the local file is real, but its replica has never received a
// backup. That must pass, not be mistaken for divergence.
func TestPreflightAllowsLocalFileWithNoReplicaYet(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, dbPath)

	if err := m.Preflight(context.Background(), "core", dbPath); err != nil {
		t.Fatalf("Preflight = %v, want nil (first run with an existing DB, no replica yet)", err)
	}
}

// TestPreflightAllowsAbsentLocalFile covers "nothing local to conflict
// with": no local file means there is nothing Preflight needs to protect.
func TestPreflightAllowsAbsentLocalFile(t *testing.T) {
	m, home := newTestManager(t)
	dbPath := filepath.Join(home, "repos", "core.db")

	if err := m.Preflight(context.Background(), "core", dbPath); err != nil {
		t.Fatalf("Preflight = %v, want nil (nothing local to conflict with)", err)
	}
}
