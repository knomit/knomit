//go:build fts5

package store_test

import (
	"path/filepath"
	"testing"

	git "knomit/internal/git"
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

func TestIncrementalSync(t *testing.T) {
	// Create a real GitStore backed by a temp dir.
	dir := t.TempDir()
	gitStore, err := git.Init(filepath.Join(dir, "test.git.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer gitStore.Close()

	// Write two fact files to the git store.
	fact1 := "---\ndomain: [databases]\nconfidence: 0.9\nsources: 2\nentities: [postgres]\nrefs: []\n---\n# Postgres MVCC\n\nPostgres uses multi-version concurrency control.\n"
	fact2 := "---\ndomain: [caching]\nconfidence: 0.8\nsources: 1\nentities: [redis]\nrefs: []\n---\n# Redis Persistence\n\nRedis supports AOF and RDB persistence.\n"

	if err := gitStore.WriteFile("know/postgres-mvcc.md", fact1, "add postgres fact"); err != nil {
		t.Fatal(err)
	}
	if err := gitStore.WriteFile("know/redis-persistence.md", fact2, "add redis fact"); err != nil {
		t.Fatal(err)
	}

	// Create a fresh in-memory search index and run a full sync.
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Sync(gitStore); err != nil {
		t.Fatalf("Sync (full rebuild) failed: %v", err)
	}

	// Both facts should now be searchable.
	results, err := idx.SearchText("concurrency", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected postgres fact after full sync")
	}
	if results[0].Path != "know/postgres-mvcc.md" {
		t.Fatalf("unexpected path %q", results[0].Path)
	}

	results, err = idx.SearchText("AOF", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected redis fact after full sync")
	}

	// Verify last_commit was set.
	head, err := gitStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	last, err := idx.GetLastCommit()
	if err != nil {
		t.Fatal(err)
	}
	if last != head {
		t.Fatalf("expected last_commit=%q, got %q", head, last)
	}

	// --- Incremental sync ---
	// Write a third fact. Sync should only index the delta.
	fact3 := "---\ndomain: [messaging]\nconfidence: 0.95\nsources: 3\nentities: [kafka]\nrefs: []\n---\n# Kafka Partitions\n\nKafka topics are split into partitions for parallelism.\n"
	if err := gitStore.WriteFile("know/kafka-partitions.md", fact3, "add kafka fact"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Sync(gitStore); err != nil {
		t.Fatalf("Sync (incremental) failed: %v", err)
	}

	// New fact should be searchable.
	results, err = idx.SearchText("partitions", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected kafka fact after incremental sync")
	}
	if results[0].Path != "know/kafka-partitions.md" {
		t.Fatalf("unexpected path %q", results[0].Path)
	}

	// Previously indexed facts should still be present.
	results, err = idx.SearchText("concurrency", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected postgres fact to survive incremental sync")
	}

	// --- Delete sync ---
	// Delete the redis fact and sync; it should be removed from the index.
	if err := gitStore.DeleteFile("know/redis-persistence.md", "delete: remove redis fact"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Sync(gitStore); err != nil {
		t.Fatalf("Sync (delete) failed: %v", err)
	}

	results, err = idx.SearchText("AOF", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected redis fact to be removed after delete sync, got %d results", len(results))
	}

	// No-op sync: calling Sync again with same HEAD should be a no-op.
	headAfter, err := gitStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Sync(gitStore); err != nil {
		t.Fatalf("Sync (no-op) failed: %v", err)
	}
	lastAfter, err := idx.GetLastCommit()
	if err != nil {
		t.Fatal(err)
	}
	if lastAfter != headAfter {
		t.Fatalf("no-op sync changed last_commit unexpectedly")
	}
}

// stubEmb is a minimal Embedder implementation that always returns a fixed
// 384-dimensional float32 vector, useful for testing without real ONNX inference.
type stubEmb struct{ vec []float32 }

func (s *stubEmb) Embed(_ string) ([]float32, error) { return s.vec, nil }

func TestGetEmbedding(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Should return nil, nil for nonexistent path.
	vec, err := idx.GetEmbedding("nonexistent.md")
	if err != nil {
		t.Fatal(err)
	}
	if vec != nil {
		t.Fatal("expected nil embedding for nonexistent path")
	}

	// Build a known 384-dim stub vector.
	const dims = 384
	known := make([]float32, dims)
	for i := range known {
		known[i] = float32(i) * 0.001
	}

	idx.SetEmbedder(&stubEmb{vec: known})

	rec := store.FactRecord{
		Path:       "know/test/emb.md",
		Title:      "Embedding test",
		Body:       "body text for embedding",
		Domain:     []string{"test"},
		Entities:   []string{},
		Confidence: 1.0,
		Sources:    1,
		CommitHash: "emb1",
	}
	if err := idx.Upsert(rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := idx.GetEmbedding("know/test/emb.md")
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if len(got) != dims {
		t.Fatalf("expected %d-dim vector, got %d", dims, len(got))
	}
	for i, v := range got {
		if v != known[i] {
			t.Fatalf("vector mismatch at index %d: got %v, want %v", i, v, known[i])
		}
	}
}
