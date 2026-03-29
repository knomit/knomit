package store_test

import (
	"testing"

	"knomit/internal/store"
)

func TestJunctionTablesPopulatedOnUpsert(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.TestDB(), "bh1", "test content")
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: "bh1",
		Type: "observation", Domain: []string{"go", "testing"}, Entities: []string{"net/http", "encoding/json"},
		Confidence: 0.8, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	var entityCount int
	if err := idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_entities WHERE fact_path = 'kb/test.md'`).Scan(&entityCount); err != nil {
		t.Fatal(err)
	}
	if entityCount != 2 {
		t.Errorf("fact_entities count = %d, want 2", entityCount)
	}

	var domainCount int
	if err := idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_domains WHERE fact_path = 'kb/test.md'`).Scan(&domainCount); err != nil {
		t.Fatal(err)
	}
	if domainCount != 2 {
		t.Errorf("fact_domains count = %d, want 2", domainCount)
	}
}

func TestJunctionTablesUpdatedOnReUpsert(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.TestDB(), "bh1", "test content")
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: "bh1",
		Type: "observation", Domain: []string{"go"}, Entities: []string{"net/http"},
		Confidence: 0.8, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with different entities and domains.
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: "bh1",
		Type: "observation", Domain: []string{"rust", "wasm"}, Entities: []string{"tokio"},
		Confidence: 0.9, Sources: 2, CommitHash: "def",
	}); err != nil {
		t.Fatal(err)
	}

	var entityCount int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_entities WHERE fact_path = 'kb/test.md'`).Scan(&entityCount)
	if entityCount != 1 {
		t.Errorf("fact_entities count after re-upsert = %d, want 1", entityCount)
	}

	var entity string
	idx.TestDB().QueryRow(`SELECT entity FROM fact_entities WHERE fact_path = 'kb/test.md'`).Scan(&entity)
	if entity != "tokio" {
		t.Errorf("entity = %q, want tokio", entity)
	}

	var domainCount int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_domains WHERE fact_path = 'kb/test.md'`).Scan(&domainCount)
	if domainCount != 2 {
		t.Errorf("fact_domains count after re-upsert = %d, want 2", domainCount)
	}
}

func TestJunctionTablesEmptyEntitiesAndDomains(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.TestDB(), "bh1", "test content")
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: "bh1",
		Type: "observation", Domain: []string{}, Entities: []string{},
		Confidence: 0.8, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	var entityCount int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_entities WHERE fact_path = 'kb/test.md'`).Scan(&entityCount)
	if entityCount != 0 {
		t.Errorf("fact_entities count = %d, want 0 for empty entities", entityCount)
	}

	var domainCount int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_domains WHERE fact_path = 'kb/test.md'`).Scan(&domainCount)
	if domainCount != 0 {
		t.Errorf("fact_domains count = %d, want 0 for empty domains", domainCount)
	}
}

func TestJunctionTablesCascadeOnDelete(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.TestDB(), "bh1", "test content")
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: "bh1",
		Type: "observation", Domain: []string{"go"}, Entities: []string{"net/http"},
		Confidence: 0.8, Sources: 1, CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	// Delete the fact directly.
	if err := idx.Delete("kb/test.md"); err != nil {
		t.Fatal(err)
	}

	var entityCount int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_entities WHERE fact_path = 'kb/test.md'`).Scan(&entityCount)
	if entityCount != 0 {
		t.Errorf("fact_entities count after delete = %d, want 0 (CASCADE)", entityCount)
	}

	var domainCount int
	idx.TestDB().QueryRow(`SELECT COUNT(*) FROM fact_domains WHERE fact_path = 'kb/test.md'`).Scan(&domainCount)
	if domainCount != 0 {
		t.Errorf("fact_domains count after delete = %d, want 0 (CASCADE)", domainCount)
	}
}
