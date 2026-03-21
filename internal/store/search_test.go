package store_test

import (
	"testing"

	"knomit/internal/store"
)

func TestSearchIncludeTypes(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{
		IncludeTypes: []string{"observation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Type != "observation" {
		t.Fatalf("expected type observation, got %q", results[0].Type)
	}
}

func TestSearchExcludeTypes(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{
		ExcludeTypes: []string{"hypothesis"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Type == "hypothesis" {
			t.Fatal("hypothesis should have been excluded")
		}
	}
}

func TestSearchTypeFilterEmptyPassesAll(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

func TestSearchIncludeMultipleTypes(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	facts := []struct {
		path     string
		blobHash string
		typ      string
		body     string
	}{
		{"kb/obs.md", "blob_obs", "observation", "observing something"},
		{"kb/hyp.md", "blob_hyp", "hypothesis", "a hypothesis about X"},
		{"kb/syn.md", "blob_syn", "synthesis", "synthesized knowledge"},
		{"kb/dec.md", "blob_dec", "decision", "a decision was made"},
	}

	for _, f := range facts {
		insertTestBlob(t, idx.DB(), f.blobHash, f.body)
		if err := idx.Upsert(store.FactRecord{
			Path:       f.path,
			Title:      f.typ + " fact",
			BlobHash:   f.blobHash,
			Type:       f.typ,
			Domain:     []string{"test"},
			Entities:   []string{},
			Confidence: 0.9,
			Sources:    1,
			CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	results, err := idx.Search(store.SearchQuery{
		IncludeTypes: []string{"observation", "hypothesis"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	types := map[string]bool{}
	for _, r := range results {
		types[r.Type] = true
	}
	if !types["observation"] || !types["hypothesis"] {
		t.Fatalf("expected observation and hypothesis, got %v", types)
	}
}

func TestMatchesFiltersWithTypes(t *testing.T) {
	// Test matchesFilters via Search with combined filters.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "blob_a", "content a")
	insertTestBlob(t, idx.DB(), "blob_b", "content b")

	if err := idx.Upsert(store.FactRecord{
		Path: "kb/a.md", Title: "A", BlobHash: "blob_a", Type: "observation",
		Domain: []string{"test"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/b.md", Title: "B", BlobHash: "blob_b", Type: "hypothesis",
		Domain: []string{"test"}, Entities: []string{}, Confidence: 0.5, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	// Exclude hypothesis + min confidence filter
	results, err := idx.Search(store.SearchQuery{
		ExcludeTypes:  []string{"hypothesis"},
		MinConfidence: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Type != "observation" {
		t.Fatalf("expected observation, got %q", results[0].Type)
	}
}
