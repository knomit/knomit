package backup

import (
	"context"
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
	rep := m.RestoreRepos(context.Background(), intended)
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

	rep := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
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

	rep := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if len(rep.Restored) != 1 || rep.Restored[0] != "core" {
		t.Fatalf("Restored = %v, want [core]", rep.Restored)
	}
	assertDBHasHello(t, dbPath)
}

// TestRestoreReposReportsGenuineFailureSeparately exercises the `case err !=
// nil` branch of RestoreRepos. It must never be confused with NoSnapshot: a real
// error (here, a broken filesystem path) has to land in Failed, not be silently
// treated as "first boot is normal."
//
// Neither outcome refuses the boot — both repos are rebuilt from their origins —
// but the distinction is what app.Bootstrap logs at ERROR rather than INFO, and
// it is the same classification that decides whether control.db may be
// replicated at all (see BootResult.ReplicateControl).
func TestRestoreReposReportsGenuineFailureSeparately(t *testing.T) {
	m, home := newTestManager(t)

	// Make repos/ a regular file so MkdirAll(filepath.Dir(dst)) for
	// <home>/repos/core.db fails with a real, non-sentinel filesystem error —
	// nothing litestream-related, nothing that isNoSnapshot recognizes.
	reposPath := filepath.Join(home, "repos")
	if err := os.WriteFile(reposPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
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

	rep := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
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

	rep := m.RestoreRepos(context.Background(), []repos.RepoRecord{{Name: "core", State: repos.RepoActive}})
	if _, ok := rep.Failed["core"]; !ok {
		t.Fatalf("Failed = %v, want core present — an unclearable orphan must refuse, not restore", rep.Failed)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Error("restored the database anyway, into a path still holding foreign WAL frames")
	}
}

// TestRestoreReposRefusesAReservedName is the last line of the reserved-name
// defence, and the one that guards the worst outcome of the three.
//
// A registry row named "control" — hand-edited, or written by a knomit older
// than the reservation — makes the two halves of the restore disagree: dst is
// derived from the repo name (<home>/repos/control.db) while the replica path
// comes from relFor, which maps "control" to the REGISTRY DATABASE. Without the
// guard, RestoreRepos happily downloads control.db's bytes into a repo file, and
// reports it as a successful restore.
//
// The replica here really does hold control.db, so this is the live hazard and
// not a shape of it: remove the guard and the assertions below fail on a file
// that exists and parses.
func TestRestoreReposRefusesAReservedName(t *testing.T) {
	m, home := newTestManager(t)

	controlPath := filepath.Join(home, "control.db")
	makeDBWithValue(t, controlPath, "registry-rows")
	if err := m.Track("control", controlPath); err != nil {
		t.Fatalf("Track control: %v", err)
	}
	waitInSync(t, m, "control")
	if err := m.Untrack("control"); err != nil {
		t.Fatalf("Untrack control: %v", err)
	}

	rep := m.RestoreRepos(context.Background(), []repos.RepoRecord{
		{Name: "control", State: repos.RepoActive},
	})
	if len(rep.Restored) != 0 {
		t.Errorf("Restored = %v, want empty — a reserved name must never be restored as a repo", rep.Restored)
	}
	if len(rep.Failed) != 0 {
		t.Errorf("Failed = %v, want empty — the row is skipped, not treated as a broken restore", rep.Failed)
	}
	if len(rep.NoSnapshot) != 0 {
		t.Errorf("NoSnapshot = %v, want empty", rep.NoSnapshot)
	}
	if _, err := os.Stat(filepath.Join(home, "repos", "control.db")); !os.IsNotExist(err) {
		t.Fatalf("repos/control.db exists (%v); the registry database's bytes were written into a repo path", err)
	}
}
