//go:build sqlite_fts5

package store_test

import (
	"path/filepath"
	"testing"

	git "knomit/internal/git"
	"knomit/internal/store"
	"go.uber.org/mock/gomock"
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

func TestVec0Available(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	var version string
	err = idx.DB().QueryRow("SELECT vec_version()").Scan(&version)
	if err != nil {
		t.Fatalf("vec_version() failed: %v — sqlite-vec not registered", err)
	}
	if version == "" {
		t.Fatal("vec_version() returned empty string")
	}
	t.Logf("sqlite-vec version: %s", version)
}

func TestGetEmbedding(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
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

	// Build a known 4-dim stub vector.
	const dims = 4
	known := make([]float32, dims)
	for i := range known {
		known[i] = float32(i) * 0.001
	}

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return(known, nil).AnyTimes()
	idx.SetEmbedder(emb)

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

// TestSearchHyphenatedEntity is a regression test: searching for a hyphenated
// entity name (e.g. "ml-pipeline") must return facts that have it as an entity.
// Before the fix, FTS5 interpreted the hyphen as a NOT operator, so facts with
// entity "ml-pipeline" were never returned.
func TestSearchHyphenatedEntity(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Upsert(store.FactRecord{
		Path:       "know/ml-pipeline.md",
		Title:      "ML Pipeline",
		Body:       "An end-to-end machine learning pipeline",
		Domain:     []string{"machine-learning"},
		Entities:   []string{"ml-pipeline", "pytorch"},
		Confidence: 0.9,
		Sources:    1,
		CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}

	// Searching by the hyphenated entity must return the fact.
	results, err := idx.Search(store.SearchQuery{Text: "ml-pipeline", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected fact with hyphenated entity 'ml-pipeline' to be found")
	}
	if results[0].Path != "know/ml-pipeline.md" {
		t.Fatalf("wrong result: %v", results[0].Path)
	}

	// Searching by a hyphenated domain must also work.
	results, err = idx.Search(store.SearchQuery{Text: "machine-learning", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected fact with hyphenated domain 'machine-learning' to be found")
	}
}

// TestSearchAllMatchesReturned is a regression test: all FTS5-matching facts must
// be returned even when one fact matches a query term more frequently than others.
// Before the fix, a normalised-score cutoff of 10% caused weak-but-valid matches
// (e.g. a fact that mentions "ml" once vs another that mentions it many times) to
// be silently dropped.
func TestSearchAllMatchesReturned(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Fact A: domain "ml", body mentions "ml" many times → high BM25 rank.
	if err := idx.Upsert(store.FactRecord{
		Path:  "know/ml-heavy.md",
		Title: "ML Overview",
		Body:  "ml ml ml ml ml ml ml ml machine learning",
		Domain: []string{"ml"}, Entities: []string{},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	// Fact B: domain "ml", body mentions "ml" only once → much lower BM25 rank.
	if err := idx.Upsert(store.FactRecord{
		Path:  "know/ml-light.md",
		Title: "Neural Networks",
		Body:  "a brief note about ml",
		Domain: []string{"ml"}, Entities: []string{},
		Confidence: 0.8, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(store.SearchQuery{Text: "ml", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both facts to be returned, got %d", len(results))
	}
}

// ── Search tests ──────────────────────────────────────────────────────────────

func TestSearch(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Upsert(store.FactRecord{
		Path: "know/a.md", Title: "Alpha", Body: "postgres database replication",
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "know/b.md", Title: "Beta", Body: "redis cache cluster",
		Domain: []string{"infra"}, Entities: []string{"redis"},
		Confidence: 0.8, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(store.SearchQuery{Text: "postgres", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Path != "know/a.md" {
		t.Fatalf("wrong result: %v", results[0].Path)
	}
	if results[0].Score < 10 {
		t.Fatalf("score too low: %v", results[0].Score)
	}
}

func TestSearchFilter(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Upsert(store.FactRecord{
		Path: "know/a.md", Title: "Alpha", Body: "postgres database replication",
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "know/b.md", Title: "Beta", Body: "redis cache cluster",
		Domain: []string{"infra"}, Entities: []string{"redis"},
		Confidence: 0.8, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	// Text-less search filtered by domain should return only the matching fact.
	results, err := idx.Search(store.SearchQuery{Domain: []string{"databases"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Path != "know/a.md" {
		t.Fatalf("expected know/a.md, got %v", results[0].Path)
	}

	// Text-less search filtered by entity.
	results, err = idx.Search(store.SearchQuery{Entities: []string{"redis"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for entity filter, got %d", len(results))
	}
	if results[0].Path != "know/b.md" {
		t.Fatalf("expected know/b.md, got %v", results[0].Path)
	}

	// Path filter should return only the fact whose path starts with "know/a".
	results, err = idx.Search(store.SearchQuery{Path: "know/a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for path filter, got %d", len(results))
	}
	if results[0].Path != "know/a.md" {
		t.Fatalf("expected know/a.md, got %v", results[0].Path)
	}

	// MinConfidence filter should drop low-confidence records.
	results, err = idx.Search(store.SearchQuery{MinConfidence: 0.85, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for confidence filter, got %d", len(results))
	}
	if results[0].Path != "know/a.md" {
		t.Fatalf("expected know/a.md, got %v", results[0].Path)
	}
}

func TestSearchHybrid(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	const dims = 4 // tiny dimension for test speed

	// Fact A: matches "postgres" in text; embedding points toward [1,0,0,0].
	vecA := []float32{1, 0, 0, 0}
	// Fact B: matches "postgres" in text too; embedding is related but less similar.
	vecB := []float32{0.7, 0.7, 0, 0}

	// Build a dispatch embedder that maps document bodies to their vectors,
	// and the query "postgres" to vecA (so fact A gets cosine sim 1, fact B gets ~0.7).
	m := map[string][]float32{
		"postgres database replication": vecA,
		"postgres cache storage":        vecB,
		"postgres":                      vecA, // query text
	}
	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if v, ok := m[text]; ok {
			return v, nil
		}
		return make([]float32, dims), nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	if err := idx.Upsert(store.FactRecord{
		Path: "know/a.md", Title: "Alpha", Body: "postgres database replication",
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "know/b.md", Title: "Beta", Body: "postgres cache storage",
		Domain: []string{"infra"}, Entities: []string{"postgres"},
		Confidence: 0.8, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(store.SearchQuery{Text: "postgres", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results from hybrid search")
	}
	// Both facts should be returned.
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Fact A should rank first because its vector exactly matches the query vector.
	if results[0].Path != "know/a.md" {
		t.Fatalf("expected know/a.md first, got %v", results[0].Path)
	}
	if results[0].Score < 10 {
		t.Fatalf("score too low: %v", results[0].Score)
	}
	// Fact A must have a strictly higher score than fact B.
	if results[0].Score <= results[1].Score {
		t.Fatalf("expected results[0].Score (%v) > results[1].Score (%v)", results[0].Score, results[1].Score)
	}
}

func TestDeleteReferentialIntegrity(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return([]float32{1, 0, 0, 0}, nil).AnyTimes()
	idx.SetEmbedder(emb)

	rec := store.FactRecord{
		Path: "know/test/ri.md", Title: "RI Test", Body: "referential integrity",
		Domain: []string{"test"}, Entities: []string{},
		Confidence: 1.0, Sources: 1, CommitHash: "ri1",
	}
	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// Verify embedding exists.
	vec, err := idx.GetEmbedding("know/test/ri.md")
	if err != nil {
		t.Fatal(err)
	}
	if vec == nil {
		t.Fatal("expected embedding after upsert")
	}

	// Delete the fact.
	if err := idx.Delete("know/test/ri.md"); err != nil {
		t.Fatal(err)
	}

	// Embedding must be gone.
	vec, err = idx.GetEmbedding("know/test/ri.md")
	if err != nil {
		t.Fatal(err)
	}
	if vec != nil {
		t.Fatal("expected nil embedding after delete — orphaned facts_vec row")
	}

	// facts_vec should be empty.
	var count int
	if err := idx.DB().QueryRow("SELECT count(*) FROM facts_vec").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected 0 rows in facts_vec after delete, got %d", count)
	}
}

// ── Similarity search tests ──────────────────────────────────────────────────

// setupSimilarityIndex creates an in-memory index with 3 facts and a mock
// embedder that assigns orthogonal vectors to each fact, allowing controlled
// cosine similarity testing.
func setupSimilarityIndex(t *testing.T) (*store.Index, *gomock.Controller) {
	t.Helper()
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}

	// Vectors: tea=[1,0,0,0], music=[0,1,0,0], code=[0,0,1,0]
	vecs := map[string][]float32{
		"Carol drinks green tea exclusively":               {1, 0, 0, 0},
		"Bob listens to jazz regularly":                    {0, 1, 0, 0},
		"Alice writes Python every day":                    {0, 0, 1, 0},
		"who likes tea":                                    {0.9, 0.1, 0, 0}, // close to tea
		"music preferences":                                {0.1, 0.9, 0, 0}, // close to music
		"who likes guns":                                   {0.3, 0.3, 0.3, 0.1}, // no strong match
	}

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if v, ok := vecs[text]; ok {
			return v, nil
		}
		return []float32{0.25, 0.25, 0.25, 0.25}, nil // default: equidistant
	}).AnyTimes()
	idx.SetEmbedder(emb)

	facts := []store.FactRecord{
		{Path: "know/people/carol/tea.md", Title: "Tea Preference", Body: "Carol drinks green tea exclusively",
			Domain: []string{"preferences"}, Entities: []string{"carol"}, Confidence: 0.9, Sources: 1, CommitHash: "a"},
		{Path: "know/people/bob/jazz.md", Title: "Jazz Fan", Body: "Bob listens to jazz regularly",
			Domain: []string{"preferences"}, Entities: []string{"bob"}, Confidence: 0.8, Sources: 1, CommitHash: "a"},
		{Path: "know/people/alice/python.md", Title: "Python Dev", Body: "Alice writes Python every day",
			Domain: []string{"engineering"}, Entities: []string{"alice"}, Confidence: 0.9, Sources: 2, CommitHash: "a"},
	}
	for _, f := range facts {
		if err := idx.Upsert(f); err != nil {
			t.Fatal(err)
		}
	}

	return idx, ctrl
}

func TestSearchSimilarityRanking(t *testing.T) {
	idx, ctrl := setupSimilarityIndex(t)
	defer idx.Close()
	defer ctrl.Finish()

	results, err := idx.Search(store.SearchQuery{Text: "who likes tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'who likes tea'")
	}
	if results[0].Path != "know/people/carol/tea.md" {
		t.Fatalf("expected tea fact first, got %v", results[0].Path)
	}
}

func TestSearchSimilarityScoreIsAbsolute(t *testing.T) {
	idx, ctrl := setupSimilarityIndex(t)
	defer idx.Close()
	defer ctrl.Finish()

	results, err := idx.Search(store.SearchQuery{Text: "who likes tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}

	// Score should reflect absolute cosine similarity, not be normalized to 100.
	// The query vector [0.9,0.1,0,0] vs tea [1,0,0,0] has cosine ~0.99.
	// Score = cosine * 100, so should be near 99, not exactly 100.
	if results[0].Score > 100 {
		t.Fatalf("score should not exceed 100, got %v", results[0].Score)
	}
	// With absolute scoring, a weak match should have a correspondingly low score.
	if len(results) > 1 && results[len(results)-1].Score > 80 {
		t.Fatalf("weakest result should have low absolute score, got %v", results[len(results)-1].Score)
	}
}

func TestSearchMinSimilarityThreshold(t *testing.T) {
	idx, ctrl := setupSimilarityIndex(t)
	defer idx.Close()
	defer ctrl.Finish()

	// Default threshold (0.40): "who likes guns" has weak cosine to all facts.
	// Vector [0.3,0.3,0.3,0.1] vs [1,0,0,0] = cosine ~0.53 — above default 0.40.
	results, err := idx.Search(store.SearchQuery{Text: "who likes guns", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	defaultCount := len(results)

	// High threshold should return fewer or no results.
	results, err = idx.Search(store.SearchQuery{Text: "who likes guns", MinSimilarity: 0.90, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) >= defaultCount && defaultCount > 0 {
		t.Fatalf("high MinSimilarity should filter more results: default=%d, high=%d", defaultCount, len(results))
	}

	// Very low threshold should return more results.
	results, err = idx.Search(store.SearchQuery{Text: "who likes guns", MinSimilarity: 0.01, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < defaultCount {
		t.Fatalf("low MinSimilarity should return at least as many results: low=%d, default=%d", len(results), defaultCount)
	}
}

func TestSearchFTSOnlySuppressedWithEmbedder(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Insert a fact WITHOUT embeddings first (no embedder set yet).
	if err := idx.Upsert(store.FactRecord{
		Path: "know/a.md", Title: "Tea Lover", Body: "tea drinking habits",
		Domain: []string{"pref"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	// Without embedder: FTS-only results should be returned.
	results, err := idx.Search(store.SearchQuery{Text: "tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected FTS results without embedder")
	}

	// Now attach an embedder. FTS-only results should be suppressed.
	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).Return([]float32{0.5, 0.5, 0, 0}, nil).AnyTimes()
	idx.SetEmbedder(emb)

	results, err = idx.Search(store.SearchQuery{Text: "tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// The fact has no embedding in facts_vec (was inserted without embedder),
	// so vec0 won't find it. With embedder set, FTS-only results are suppressed.
	if len(results) != 0 {
		t.Fatalf("expected 0 results (FTS-only suppressed with embedder), got %d", len(results))
	}
}

func TestSearchHybridScoringBoost(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Two facts: both have embeddings, but only one matches FTS.
	vecs := map[string][]float32{
		"tea brewing techniques":  {1, 0, 0, 0},
		"tea garden cultivation":  {0.95, 0.05, 0, 0}, // slightly less similar
		"tea":                     {1, 0, 0, 0},        // query
	}

	ctrl := gomock.NewController(t)
	emb := NewMockEmbedder(ctrl)
	emb.EXPECT().Embed(gomock.Any()).DoAndReturn(func(text string) ([]float32, error) {
		if v, ok := vecs[text]; ok {
			return v, nil
		}
		return []float32{0.25, 0.25, 0.25, 0.25}, nil
	}).AnyTimes()
	idx.SetEmbedder(emb)

	// Fact A: exact embedding match + FTS match on "tea"
	if err := idx.Upsert(store.FactRecord{
		Path: "know/a.md", Title: "Brewing", Body: "tea brewing techniques",
		Domain: []string{"food"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	// Fact B: very close embedding but body text doesn't match FTS for "tea" well
	if err := idx.Upsert(store.FactRecord{
		Path: "know/b.md", Title: "Garden", Body: "tea garden cultivation",
		Domain: []string{"food"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(store.SearchQuery{Text: "tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Both should have scores > 40 (cosine threshold).
	for _, r := range results {
		if r.Score <= 40 {
			t.Fatalf("score too low for fact %s: %v", r.Path, r.Score)
		}
	}
	// Fact A (exact cosine + FTS boost) should score higher than B (slightly lower cosine).
	if results[0].Score <= results[1].Score {
		t.Fatalf("expected results[0] > results[1], got %v <= %v", results[0].Score, results[1].Score)
	}
}
