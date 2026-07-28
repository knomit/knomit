package repos

import (
	"path/filepath"
	"testing"
	"time"
)

func openRegistry(t *testing.T) *RepoRegistry {
	t.Helper()
	r, err := OpenRepoRegistry(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("OpenRepoRegistry: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRegistryUpsertAndList(t *testing.T) {
	r := openRegistry(t)
	rec := RepoRecord{
		Name:         "core",
		OriginURL:    "git@github.com:acme/kb.git",
		OriginBranch: "main",
		State:        RepoActive,
		CreatedAt:    time.Unix(1700000000, 0).UTC(),
	}
	if err := r.Upsert(rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := r.List(RepoActive)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(List) = %d, want 1", len(got))
	}
	if got[0].Name != "core" || got[0].OriginURL != rec.OriginURL || got[0].OriginBranch != "main" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
	if !got[0].CreatedAt.Equal(rec.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got[0].CreatedAt, rec.CreatedAt)
	}
}

func TestRegistryUpsertIsUpdateOnConflict(t *testing.T) {
	r := openRegistry(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.Upsert(RepoRecord{Name: "core", OriginURL: "old", State: RepoActive}))
	must(r.Upsert(RepoRecord{Name: "core", OriginURL: "new", State: RepoActive}))

	got, err := r.List("")
	must(err)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (upsert must not duplicate)", len(got))
	}
	if got[0].OriginURL != "new" {
		t.Errorf("OriginURL = %q, want %q", got[0].OriginURL, "new")
	}
}

func TestRegistryStateFilter(t *testing.T) {
	r := openRegistry(t)
	_ = r.Upsert(RepoRecord{Name: "core", State: RepoActive})
	_ = r.Upsert(RepoRecord{Name: "old", State: RepoArchived, ArchiveID: "arc-1"})

	active, err := r.List(RepoActive)
	if err != nil {
		t.Fatalf("List(active): %v", err)
	}
	if len(active) != 1 || active[0].Name != "core" {
		t.Errorf("active = %+v, want just core", active)
	}

	archived, err := r.List(RepoArchived)
	if err != nil {
		t.Fatalf("List(archived): %v", err)
	}
	if len(archived) != 1 || archived[0].ArchiveID != "arc-1" {
		t.Errorf("archived = %+v, want just old/arc-1", archived)
	}

	all, err := r.List("")
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("len(all) = %d, want 2", len(all))
	}
}

func TestRegistrySetStateAndDelete(t *testing.T) {
	r := openRegistry(t)
	_ = r.Upsert(RepoRecord{Name: "core", State: RepoActive})

	if err := r.SetState("core", RepoArchived); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, _ := r.List(RepoArchived)
	if len(got) != 1 {
		t.Fatalf("SetState did not move the row: %+v", got)
	}

	if err := r.Delete("core"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	all, _ := r.List("")
	if len(all) != 0 {
		t.Errorf("len after Delete = %d, want 0", len(all))
	}
}
