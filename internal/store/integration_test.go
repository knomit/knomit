package store

import (
	"path/filepath"
	"testing"

	"knomit/internal/git"
)

// openTestService creates a temporary Service for testing.
func openTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	svc, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

func TestDeleteFactAtomically(t *testing.T) {
	svc := openTestService(t)

	gs, err := git.InitWithStorer(svc.GitStorer(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Write a fact
	_, blobHash, err := gs.WriteFile("kb/test.md", "---\ndomain: []\nconfidence: 1\nsources: 1\nentities: []\nrefs: []\n---\n# Test\n\nBody.", "add test")
	if err != nil {
		t.Fatal(err)
	}

	// Upsert to index
	rec := FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: blobHash,
		Domain: []string{}, Entities: []string{}, Confidence: 1, Sources: 1, Refs: []string{},
		CommitHash: "abc",
	}
	if err := svc.Index().Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// Delete
	if err := svc.DeleteFact(gs, "kb/test.md", "forget test"); err != nil {
		t.Fatal(err)
	}

	// Verify: fact gone from index
	got, _ := svc.Index().GetByPath("kb/test.md")
	if got != nil {
		t.Fatal("expected fact to be deleted from index")
	}

	// Verify: file gone from git
	exists, _ := gs.FileExists("kb/test.md")
	if exists {
		t.Fatal("expected file to be deleted from git")
	}
}

func TestFullRoundtrip(t *testing.T) {
	svc := openTestService(t)

	gs, err := git.InitWithStorer(svc.GitStorer(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Write a fact via git
	content := "---\ndomain: [databases]\nconfidence: 0.9\nsources: 1\nentities: [postgres]\nrefs: []\n---\n# Postgres is great\n\nPostgreSQL is a powerful RDBMS."
	_, blobHash, err := gs.WriteFile("kb/db/postgres.md", content, "learn postgres")
	if err != nil {
		t.Fatal(err)
	}

	// Upsert to index
	rec := FactRecord{
		Path: "kb/db/postgres.md", Title: "Postgres is great", BlobHash: blobHash,
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, Refs: []string{},
		CommitHash: "abc123",
	}
	if err := svc.Index().Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// Read back with body hydrated from git objects
	got, err := svc.Index().GetByPath("kb/db/postgres.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected fact")
	}
	if got.Body != "PostgreSQL is a powerful RDBMS." {
		t.Fatalf("unexpected body: %q", got.Body)
	}
	if got.BlobHash != blobHash {
		t.Fatalf("unexpected blob_hash: %q", got.BlobHash)
	}
	if got.Title != "Postgres is great" {
		t.Fatalf("unexpected title: %q", got.Title)
	}

	// Sync should work
	if err := svc.Index().Sync(gs); err != nil {
		t.Fatal(err)
	}

	// Delete
	if err := svc.DeleteFact(gs, "kb/db/postgres.md", "forget postgres"); err != nil {
		t.Fatal(err)
	}

	// Verify gone
	got, _ = svc.Index().GetByPath("kb/db/postgres.md")
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}
