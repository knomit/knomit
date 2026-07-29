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
