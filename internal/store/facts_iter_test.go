package store_test

import (
	"testing"

	"knomit/internal/store"
)

func openTestService(t *testing.T) *store.Service {
	t.Helper()
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc
}

func insertFact(t *testing.T, svc *store.Service, path, blobHash, commitHash string) {
	t.Helper()
	// Insert a blob so Upsert can find it.
	_, err := svc.TestDB().Exec(
		`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, 3, 10, ?)`,
		blobHash, []byte("---\ndomain: [test]\nconfidence: 0.9\nsources: 1\n---\n# Title\n\nbody"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Index().Upsert(testBranch, commitHash, store.FactRecord{
		Path:       path,
		Title:      "title",
		BlobHash:   blobHash,
		Type:       "observation",
		Domain:     []string{"test"},
		Entities:   []string{},
		Confidence: 0.9,
		Sources:    1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFactsIter_EmptyDB(t *testing.T) {
	svc := openTestService(t)

	// Ensure the branch exists before iterating.
	if _, err := svc.Index().EnsureBranch(testBranch, "refs/heads/"+testBranch); err != nil {
		t.Fatal(err)
	}

	iter, err := store.NewFactsIter(svc.Index(), testBranch)
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	row, err := iter.Next()
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Fatalf("expected nil from empty DB, got %+v", row)
	}
}

func TestFactsIter_ReturnsAllFacts(t *testing.T) {
	svc := openTestService(t)

	insertFact(t, svc, "a/one.md", "blob1", "commit1")
	insertFact(t, svc, "b/two.md", "blob2", "commit2")
	insertFact(t, svc, "c/three.md", "blob3", "commit3")

	iter, err := store.NewFactsIter(svc.Index(), testBranch)
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	var rows []store.FactRow
	for {
		row, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			break
		}
		rows = append(rows, *row)
	}

	if len(rows) != 3 {
		t.Fatalf("expected 3 facts, got %d", len(rows))
	}

	// Newest-first (highest fact_id first): c/three.md was inserted last.
	if rows[0].Path != "c/three.md" {
		t.Errorf("expected first row path=c/three.md, got %s", rows[0].Path)
	}
	if rows[0].BlobHash != "blob3" {
		t.Errorf("expected first row blob_hash=blob3, got %s", rows[0].BlobHash)
	}
	if rows[0].CommitHash != "commit3" {
		t.Errorf("expected first row commit_hash=commit3, got %s", rows[0].CommitHash)
	}
	if rows[2].Path != "a/one.md" {
		t.Errorf("expected last row path=a/one.md, got %s", rows[2].Path)
	}
}

func TestFactsIter_DedupsByPath(t *testing.T) {
	svc := openTestService(t)

	// Insert initial version, then overwrite with newer version via Upsert.
	insertFact(t, svc, "a/one.md", "blob_old", "commit_old")
	// Overwrite: insert new blob, then Upsert with new blob hash.
	_, err := svc.TestDB().Exec(
		`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, 3, 10, ?)`,
		"blob_new", []byte("---\ndomain: [test]\nconfidence: 0.9\nsources: 1\n---\n# Title\n\nnew body"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Index().Upsert(testBranch, "commit_new", store.FactRecord{
		Path:       "a/one.md",
		Title:      "title",
		BlobHash:   "blob_new",
		Type:       "observation",
		Domain:     []string{"test"},
		Entities:   []string{},
		Confidence: 0.9,
		Sources:    1,
	}); err != nil {
		t.Fatal(err)
	}
	insertFact(t, svc, "b/two.md", "blob2", "commit2")

	iter, err := store.NewFactsIter(svc.Index(), testBranch)
	if err != nil {
		t.Fatal(err)
	}
	defer iter.Close()

	var rows []store.FactRow
	for {
		row, err := iter.Next()
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			break
		}
		rows = append(rows, *row)
	}

	if len(rows) != 2 {
		t.Fatalf("expected 2 unique paths, got %d", len(rows))
	}

	// Verify dedup: a/one.md should have the newer version.
	pathMap := make(map[string]store.FactRow)
	for _, r := range rows {
		pathMap[r.Path] = r
	}
	if r, ok := pathMap["a/one.md"]; !ok {
		t.Fatal("missing a/one.md")
	} else if r.BlobHash != "blob_new" {
		t.Errorf("expected blob_new for a/one.md, got %s", r.BlobHash)
	}
}

func TestFactsIter_CloseIsIdempotent(t *testing.T) {
	svc := openTestService(t)

	if _, err := svc.Index().EnsureBranch(testBranch, "refs/heads/"+testBranch); err != nil {
		t.Fatal(err)
	}

	iter, err := store.NewFactsIter(svc.Index(), testBranch)
	if err != nil {
		t.Fatal(err)
	}

	// Close twice should not panic or error.
	if err := iter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := iter.Close(); err != nil {
		t.Fatal(err)
	}
}
