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

// TestAdoptIsAllOrNothing pins that a failed adoption leaves the registry
// EMPTY, which is the only state from which it can be retried.
//
// Adoption is the one-time migration off filesystem-as-registry and it runs
// only while the registry is empty. Written row by row, a failure partway
// through would leave rows behind — and the emptiness gate would then skip
// adoption on every subsequent boot, permanently. The repos the failed run had
// not reached would still be sitting on disk and yet be invisible to the
// server, with nothing in the log to say they had been passed over: exactly the
// silent disappearance the registry was built to end.
//
// The fault is injected in SQL, on the SECOND row only, because that is the
// shape that distinguishes the two implementations: a row-by-row adoption would
// have committed "alpha" before dying on "beta".
func TestAdoptIsAllOrNothing(t *testing.T) {
	home := t.TempDir()
	reposDir := filepath.Join(home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"alpha.db", "beta.db"} {
		if err := os.WriteFile(filepath.Join(reposDir, f), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	reg, err := OpenRepoRegistry(filepath.Join(home, "control.db"))
	if err != nil {
		t.Fatalf("OpenRepoRegistry: %v", err)
	}
	defer reg.Close()

	if _, err := reg.db.Exec(`CREATE TRIGGER block_beta BEFORE INSERT ON repos
		WHEN NEW.name = 'beta'
		BEGIN SELECT RAISE(ABORT, 'insert blocked by test'); END`); err != nil {
		t.Fatal(err)
	}

	if n, err := adoptFromFilesystem(reg, reposDir); err == nil {
		t.Fatalf("adoptFromFilesystem succeeded (%d rows) despite a blocked insert", n)
	} else if n != 0 {
		t.Errorf("adopted = %d, want 0 — a failed adoption must report no rows", n)
	}

	all, err := reg.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("registry holds %+v after a failed adoption; a non-empty registry is never "+
			"adopted again, so alpha would be stranded on disk forever", all)
	}

	// And because the registry is still empty, the next boot picks up where
	// this one left off — the whole point of keeping it that way.
	if _, err := reg.db.Exec(`DROP TRIGGER block_beta`); err != nil {
		t.Fatal(err)
	}
	n, err := adoptFromFilesystem(reg, reposDir)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n != 2 {
		t.Errorf("retry adopted %d, want 2 (alpha, beta)", n)
	}
	active, err := reg.List(RepoActive)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Errorf("active = %+v, want alpha and beta", active)
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
