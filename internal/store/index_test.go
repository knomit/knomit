package store_test

import (
	"testing"

	"knomit/internal/store"
)

func TestUpsertAndQuery(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	err = idx.Upsert(store.FactRecord{
		Path:       "know/test/foo.md",
		Title:      "Foo fact",
		Body:       "This is about databases and postgres",
		Domain:     []string{"databases"},
		Entities:   []string{"postgres"},
		Confidence: 0.9,
		Sources:    1,
		CommitHash: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}

	results, err := idx.SearchText("postgres", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].Path != "know/test/foo.md" {
		t.Fatalf("got %v", results[0].Path)
	}
}

func TestDelete(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	rec := store.FactRecord{
		Path:       "know/test/bar.md",
		Title:      "Bar fact",
		Body:       "This is about redis and caching",
		Domain:     []string{"caching"},
		Entities:   []string{"redis"},
		Confidence: 0.8,
		Sources:    2,
		CommitHash: "def",
	}

	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	results, err := idx.SearchText("redis", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected result before delete")
	}

	if err := idx.Delete("know/test/bar.md"); err != nil {
		t.Fatal(err)
	}

	results, err = idx.SearchText("redis", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results after delete, got %d", len(results))
	}
}

func TestGetByPath(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Not found case
	rec, err := idx.GetByPath("nonexistent.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("expected nil for nonexistent path")
	}

	// Insert and retrieve
	original := store.FactRecord{
		Path:       "know/test/baz.md",
		Title:      "Baz fact",
		Body:       "This is about golang",
		Domain:     []string{"languages"},
		Entities:   []string{"golang", "go"},
		Confidence: 0.95,
		Sources:    3,
		CommitHash: "ghi",
	}

	if err := idx.Upsert(original); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetByPath("know/test/baz.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected record, got nil")
	}
	if got.Title != "Baz fact" {
		t.Fatalf("expected title 'Baz fact', got %q", got.Title)
	}
	if got.Confidence != 0.95 {
		t.Fatalf("expected confidence 0.95, got %v", got.Confidence)
	}
}

func TestUpsertOverwrite(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	rec := store.FactRecord{
		Path:       "know/test/overwrite.md",
		Title:      "Original title",
		Body:       "original body text about mysql",
		Domain:     []string{"databases"},
		Entities:   []string{"mysql"},
		Confidence: 0.7,
		Sources:    1,
		CommitHash: "v1",
	}

	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// Overwrite with updated record
	rec.Title = "Updated title"
	rec.Body = "updated body text about postgresql"
	rec.Entities = []string{"postgresql"}
	rec.CommitHash = "v2"

	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// FTS should find new content
	results, err := idx.SearchText("postgresql", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected result for new content")
	}
	if results[0].Title != "Updated title" {
		t.Fatalf("expected updated title, got %q", results[0].Title)
	}

	// FTS should NOT find old content
	results, err = idx.SearchText("mysql", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results for old content, got %d", len(results))
	}
}

func TestLastCommit(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Should be empty initially
	hash, err := idx.GetLastCommit()
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Fatalf("expected empty hash, got %q", hash)
	}

	// Set and retrieve
	if err := idx.SetLastCommit("abc123"); err != nil {
		t.Fatal(err)
	}

	hash, err = idx.GetLastCommit()
	if err != nil {
		t.Fatal(err)
	}
	if hash != "abc123" {
		t.Fatalf("expected 'abc123', got %q", hash)
	}

	// Overwrite
	if err := idx.SetLastCommit("def456"); err != nil {
		t.Fatal(err)
	}

	hash, err = idx.GetLastCommit()
	if err != nil {
		t.Fatal(err)
	}
	if hash != "def456" {
		t.Fatalf("expected 'def456', got %q", hash)
	}
}

func TestGetEmbedding(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Should return nil, nil for nonexistent path
	vec, err := idx.GetEmbedding("nonexistent.md")
	if err != nil {
		t.Fatal(err)
	}
	if vec != nil {
		t.Fatal("expected nil embedding for nonexistent path")
	}
}
