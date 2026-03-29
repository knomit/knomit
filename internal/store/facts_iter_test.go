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
	_, err := svc.DB().Exec(
		`INSERT INTO facts (path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES (?, '', ?, 'observation', '', '', 1.0, 0, '', ?)`,
		path, blobHash, commitHash,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFactsIter_EmptyDB(t *testing.T) {
	svc := openTestService(t)

	iter, err := store.NewFactsIter(svc.Index())
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

	iter, err := store.NewFactsIter(svc.Index())
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

	// Newest-first (highest rowid first): c/three.md was inserted last.
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

	// Since path is PRIMARY KEY, we simulate "multiple versions" by using
	// INSERT OR REPLACE — the latest insert wins. The iterator should still
	// return each path only once.
	insertFact(t, svc, "a/one.md", "blob_old", "commit_old")
	// Replace with newer version:
	_, err := svc.DB().Exec(
		`INSERT OR REPLACE INTO facts (path, title, blob_hash, type, domain, entities, confidence, sources, refs, commit_hash)
		 VALUES ('a/one.md', '', 'blob_new', 'observation', '', '', 1.0, 0, '', 'commit_new')`,
	)
	if err != nil {
		t.Fatal(err)
	}
	insertFact(t, svc, "b/two.md", "blob2", "commit2")

	iter, err := store.NewFactsIter(svc.Index())
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

	iter, err := store.NewFactsIter(svc.Index())
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
