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

// TestPreflightDetectsDivergedReplica covers the comparison direction in
// Preflight directly: RemoteTXID > LocalTXID must trip ErrDiverged. A `<`
// instead of `>` here would either never fire or always fire, and no other
// test in this package would notice.
//
// Approach: track a db under "core" long enough to push the replica's remote
// TXID to 1, untrack it, then run Preflight for the same name against a
// DIFFERENT, never-tracked local file. That file has no local litestream
// shadow metadata (local TXID 0), while the "core" replica already holds
// history (remote TXID 1) — exactly the "stale volume reattached" scenario
// Preflight exists to catch.
func TestPreflightDetectsDivergedReplica(t *testing.T) {
	m, home := newTestManager(t)

	trackedPath := filepath.Join(home, "repos", "core.db")
	if err := os.MkdirAll(filepath.Dir(trackedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	makeDB(t, trackedPath)
	if err := m.Track("core", trackedPath); err != nil {
		t.Fatalf("Track: %v", err)
	}
	waitInSync(t, m, "core")
	if err := m.Untrack("core"); err != nil {
		t.Fatalf("Untrack: %v", err)
	}

	stalePath := filepath.Join(home, "stale-core.db")
	makeDB(t, stalePath)

	err := m.Preflight(context.Background(), "core", stalePath)
	if !errors.Is(err, ErrDiverged) {
		t.Fatalf("Preflight = %v, want ErrDiverged", err)
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
