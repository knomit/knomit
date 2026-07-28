package repos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAdoptFromFilesystemPopulatesRegistry(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	if err := os.MkdirAll(filepath.Join(reposDir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Two repos, one session sidecar that must be ignored.
	for _, f := range []string{"core.db", "notes.db", "core.sessions.db"} {
		if err := os.WriteFile(filepath.Join(reposDir, f), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One archived repo with a legacy JSON manifest.
	info := ArchiveInfo{ID: "arc-1", Name: "old", ArchivedAt: "2026-01-01T00:00:00Z"}
	blob, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(reposDir, "archive", "arc-1.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := OpenRepoRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		t.Fatalf("OpenRepoRegistry: %v", err)
	}
	defer reg.Close()

	n, err := adoptFromFilesystem(reg, reposDir)
	if err != nil {
		t.Fatalf("adoptFromFilesystem: %v", err)
	}
	if n != 3 {
		t.Errorf("adopted %d, want 3 (core, notes, old)", n)
	}

	active, _ := reg.List(RepoActive)
	if len(active) != 2 {
		t.Errorf("active = %+v, want core and notes (session sidecar must be skipped)", active)
	}
	archived, _ := reg.List(RepoArchived)
	if len(archived) != 1 || archived[0].ArchiveID != "arc-1" {
		t.Errorf("archived = %+v, want arc-1", archived)
	}
}

func TestAdoptIsSkippedWhenRegistryNonEmpty(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reposDir, "stray.db"), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := OpenRepoRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if err := reg.Upsert(RepoRecord{Name: "core", State: RepoActive}); err != nil {
		t.Fatal(err)
	}

	n, err := adoptFromFilesystem(reg, reposDir)
	if err != nil {
		t.Fatalf("adoptFromFilesystem: %v", err)
	}
	if n != 0 {
		t.Errorf("adopted %d, want 0 — a populated registry is authoritative", n)
	}
	all, _ := reg.List("")
	if len(all) != 1 {
		t.Errorf("registry mutated: %+v", all)
	}
}
