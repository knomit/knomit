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

// TestRegistryDeleteActiveAndArchive replaces the original SetState/Delete test.
// Those two took a bare name, which stopped identifying a single row once the
// key widened to (name, archive_id) — archiving is now "insert the archived row,
// retire the active one", so the registry exposes the two deletes that operation
// actually needs. This pins that each one touches ONLY its own row: the case
// where a name carries both a live repo and an archive of an earlier repo with
// the same name is exactly where a name-keyed delete did damage.
func TestRegistryDeleteActiveAndArchive(t *testing.T) {
	r := openRegistry(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// One name, two rows: a live repo and an archive of an earlier repo that
	// happened to share the name.
	must(r.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	must(r.Upsert(RepoRecord{Name: "work", State: RepoArchived, ArchiveID: "arc-1"}))

	must(r.DeleteActive("work"))
	archived, err := r.List(RepoArchived)
	must(err)
	if len(archived) != 1 || archived[0].ArchiveID != "arc-1" {
		t.Errorf("DeleteActive took the archive with it: %+v", archived)
	}
	active, err := r.List(RepoActive)
	must(err)
	if len(active) != 0 {
		t.Errorf("DeleteActive left the active row: %+v", active)
	}

	// Put the live repo back, then purge the archive and confirm the reverse.
	must(r.Upsert(RepoRecord{Name: "work", State: RepoActive}))
	must(r.DeleteArchive("arc-1"))
	all, err := r.List("")
	must(err)
	if len(all) != 1 || all[0].State != RepoActive {
		t.Errorf("DeleteArchive must leave only the live repo, got %+v", all)
	}
}

// TestRegistryRecordLookups covers the single-row getters Archive's rollback and
// Restore's provenance carry-over depend on.
func TestRegistryRecordLookups(t *testing.T) {
	r := openRegistry(t)
	created := time.Unix(1700000000, 0).UTC()
	if err := r.Upsert(RepoRecord{Name: "work", State: RepoActive, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := r.Upsert(RepoRecord{
		Name: "work", State: RepoArchived, ArchiveID: "arc-1", CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}

	act, ok, err := r.ActiveRecord("work")
	if err != nil || !ok {
		t.Fatalf("ActiveRecord: %v ok=%v", err, ok)
	}
	if act.ArchiveID != "" || !act.CreatedAt.Equal(created) {
		t.Errorf("ActiveRecord returned the wrong row: %+v", act)
	}

	arc, ok, err := r.ArchiveRecord("arc-1")
	if err != nil || !ok {
		t.Fatalf("ArchiveRecord: %v ok=%v", err, ok)
	}
	if arc.ArchiveID != "arc-1" || !arc.CreatedAt.Equal(created) {
		t.Errorf("ArchiveRecord returned the wrong row: %+v", arc)
	}

	if _, ok, err := r.ActiveRecord("nope"); err != nil || ok {
		t.Errorf("missing name: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := r.ArchiveRecord(""); err != nil || ok {
		t.Errorf("empty archive id: ok=%v err=%v, want false/nil", ok, err)
	}
}
