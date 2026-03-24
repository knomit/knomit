package store_test

import (
	"testing"

	"knomit/internal/fact"
	"knomit/internal/store"
)

func TestNewFactRecord(t *testing.T) {
	f := fact.NewFact("kb/Alpha/test.md")
	f.Title = "Test Fact"
	f.Type = "principle"
	f.Domain = []string{"go", "testing"}
	f.Entities = []string{"net/http"}
	f.Confidence = 0.85
	f.Sources = 3
	f.Refs = []string{"https://example.com"}
	f.EvidenceWeight = 1.5

	rec := store.NewFactRecord(f, "blobhash123", "commithash456")

	if rec.Path != "kb/alpha/test.md" { // NewFact lowercases
		t.Errorf("path = %q, want %q", rec.Path, "kb/alpha/test.md")
	}
	if rec.Title != "Test Fact" {
		t.Errorf("title = %q", rec.Title)
	}
	if rec.Type != "principle" {
		t.Errorf("type = %q", rec.Type)
	}
	if len(rec.Domain) != 2 || rec.Domain[0] != "go" {
		t.Errorf("domain = %v", rec.Domain)
	}
	if len(rec.Entities) != 1 || rec.Entities[0] != "net/http" {
		t.Errorf("entities = %v", rec.Entities)
	}
	if rec.Confidence != 0.85 {
		t.Errorf("confidence = %f", rec.Confidence)
	}
	if rec.Sources != 3 {
		t.Errorf("sources = %d", rec.Sources)
	}
	if len(rec.Refs) != 1 || rec.Refs[0] != "https://example.com" {
		t.Errorf("refs = %v", rec.Refs)
	}
	if rec.BlobHash != "blobhash123" {
		t.Errorf("blob_hash = %q", rec.BlobHash)
	}
	if rec.CommitHash != "commithash456" {
		t.Errorf("commit_hash = %q", rec.CommitHash)
	}
	if rec.EvidenceWeight != 1.5 {
		t.Errorf("evidence_weight = %f", rec.EvidenceWeight)
	}
}

type noopEmbedder struct{}

func (noopEmbedder) Embed(_ string) ([]float32, error) { return []float32{1}, nil }

func TestEmbedderSet(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if idx.EmbedderSet() {
		t.Error("expected EmbedderSet=false before SetEmbedder")
	}

	idx.SetEmbedder(noopEmbedder{})

	if !idx.EmbedderSet() {
		t.Error("expected EmbedderSet=true after SetEmbedder")
	}
}
