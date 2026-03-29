package store

import (
	"path/filepath"
	"testing"

	"knomit/internal/git"
)

const testBranch = "agent/test"

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

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}
	gs.SetOnCommit(func(_, _ string) {
		if err := svc.Index().Sync(gs, testBranch); err != nil {
			t.Errorf("onCommit sync: %v", err)
		}
	})

	// Write a fact
	_, blobHash, err := gs.WriteFile(testBranch, "kb/test.md", "---\ndomain: []\nconfidence: 1\nsources: 1\nentities: []\nrefs: []\n---\n# Test\n\nBody.", "add test", "learn")
	if err != nil {
		t.Fatal(err)
	}

	// Upsert to index
	rec := FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: blobHash,
		Domain: []string{}, Entities: []string{}, Confidence: 1, Sources: 1, Refs: []string{},
		
	}
	if err := svc.Index().Upsert(testBranch, "abc", rec); err != nil {
		t.Fatal(err)
	}

	// Delete
	if err := svc.DeleteFact(gs, testBranch, "kb/test.md", "forget test"); err != nil {
		t.Fatal(err)
	}

	// Verify: fact gone from index
	got, _ := svc.Index().GetByPath(testBranch, "kb/test.md")
	if got != nil {
		t.Fatal("expected fact to be deleted from index")
	}

	// Verify: file gone from git
	exists, _ := gs.FileExists(testBranch, "kb/test.md")
	if exists {
		t.Fatal("expected file to be deleted from git")
	}
}

func TestEvidenceWeightRoundTrip(t *testing.T) {
	svc := openTestService(t)
	gs, err := git.InitWithStorer(svc.GitStorer(), nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}

	_, blobHash, err := gs.WriteFile(testBranch, "kb/weighted.md",
		"---\ndomain: []\nconfidence: 0.9\nsources: 5\nentities: []\nrefs: []\n---\n# Weighted\n\nBody.",
		"add weighted", "learn",
	)
	if err != nil {
		t.Fatal(err)
	}

	rec := FactRecord{
		Path: "kb/weighted.md", Title: "Weighted", BlobHash: blobHash,
		Domain: []string{}, Entities: []string{}, Confidence: 0.9, Sources: 5,
		Refs: []string{},  EvidenceWeight: 0.714,
	}
	if err := svc.Index().Upsert(testBranch, "abc", rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := svc.Index().GetByPath(testBranch, "kb/weighted.md")
	if err != nil {
		t.Fatalf("GetByPath: %v", err)
	}
	if got == nil {
		t.Fatal("GetByPath: got nil")
	}
	if got.EvidenceWeight != 0.714 {
		t.Fatalf("EvidenceWeight: got %v, want 0.714", got.EvidenceWeight)
	}
}

func TestFullRoundtrip(t *testing.T) {
	svc := openTestService(t)

	gs, err := git.InitWithStorer(svc.GitStorer(), nil, testBranch)
	if err != nil {
		t.Fatal(err)
	}
	gs.SetOnCommit(func(_, _ string) {
		if err := svc.Index().Sync(gs, testBranch); err != nil {
			t.Errorf("onCommit sync: %v", err)
		}
	})

	// Write a fact via git
	content := "---\ndomain: [databases]\nconfidence: 0.9\nsources: 1\nentities: [postgres]\nrefs: []\n---\n# Postgres is great\n\nPostgreSQL is a powerful RDBMS."
	_, blobHash, err := gs.WriteFile(testBranch, "kb/db/postgres.md", content, "learn postgres", "learn")
	if err != nil {
		t.Fatal(err)
	}

	// Upsert to index
	rec := FactRecord{
		Path: "kb/db/postgres.md", Title: "Postgres is great", BlobHash: blobHash,
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, Refs: []string{},
		
	}
	if err := svc.Index().Upsert(testBranch, "abc", rec); err != nil {
		t.Fatal(err)
	}

	// Read back with body hydrated from git objects
	got, err := svc.Index().GetByPath(testBranch, "kb/db/postgres.md")
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
	if err := svc.Index().Sync(gs, testBranch); err != nil {
		t.Fatal(err)
	}

	// Delete
	if err := svc.DeleteFact(gs, testBranch, "kb/db/postgres.md", "forget postgres"); err != nil {
		t.Fatal(err)
	}

	// Verify gone
	got, _ = svc.Index().GetByPath(testBranch, "kb/db/postgres.md")
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}
