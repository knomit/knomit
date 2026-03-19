package store_test

import (
	"database/sql"
	"strings"
	"testing"

	git "knomit/internal/git"
	"knomit/internal/store"
	"go.uber.org/mock/gomock"
)

// insertTestBlob inserts a fake blob into the objects table for testing.
// If content has no frontmatter, it is wrapped with a default header.
func insertTestBlob(t *testing.T, db *sql.DB, hash, content string) {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		content = "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\n---\n# Title\n\n" + content
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
		hash, 3 /* BlobObjectType */, len(content), []byte(content))
	if err != nil {
		t.Fatalf("insertTestBlob %s: %v", hash, err)
	}
}

func TestUpsertAndGetByPath(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "blob_foo", "This is about databases and postgres")

	err = idx.Upsert(store.FactRecord{
		Path:       "kb/test/foo.md",
		Title:      "Foo fact",
		BlobHash:   "blob_foo",
		Domain:     []string{"databases"},
		Entities:   []string{"postgres"},
		Confidence: 0.9,
		Sources:    1,
		CommitHash: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := idx.GetByPath("kb/test/foo.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected record, got nil")
	}
	if rec.Title != "Foo fact" {
		t.Fatalf("expected title 'Foo fact', got %q", rec.Title)
	}
}

func TestDelete(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "blob_bar", "This is about redis and caching")

	rec := store.FactRecord{
		Path:       "kb/test/bar.md",
		Title:      "Bar fact",
		BlobHash:   "blob_bar",
		Domain:     []string{"caching"},
		Entities:   []string{"redis"},
		Confidence: 0.8,
		Sources:    2,
		CommitHash: "def",
	}

	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetByPath("kb/test/bar.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected result before delete")
	}

	if err := idx.Delete("kb/test/bar.md"); err != nil {
		t.Fatal(err)
	}

	got, err = idx.GetByPath("kb/test/bar.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("expected nil after delete")
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
	insertTestBlob(t, idx.DB(), "blob_baz", "This is about golang")

	original := store.FactRecord{
		Path:       "kb/test/baz.md",
		Title:      "Baz fact",
		BlobHash:   "blob_baz",
		Domain:     []string{"languages"},
		Entities:   []string{"golang", "go"},
		Confidence: 0.95,
		Sources:    3,
		CommitHash: "ghi",
	}

	if err := idx.Upsert(original); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetByPath("kb/test/baz.md")
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

	insertTestBlob(t, idx.DB(), "blob_v1", "original body text about mysql")
	insertTestBlob(t, idx.DB(), "blob_v2", "updated body text about postgresql")

	rec := store.FactRecord{
		Path:       "kb/test/overwrite.md",
		Title:      "Original title",
		BlobHash:   "blob_v1",
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
	rec.BlobHash = "blob_v2"
	rec.Entities = []string{"postgresql"}
	rec.CommitHash = "v2"

	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	got, err := idx.GetByPath("kb/test/overwrite.md")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected record after overwrite")
	}
	if got.Title != "Updated title" {
		t.Fatalf("expected updated title, got %q", got.Title)
	}
	if got.CommitHash != "v2" {
		t.Fatalf("expected commit_hash 'v2', got %q", got.CommitHash)
	}
}

func TestLastCommit(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Should be empty initially
	hash, err := idx.GetLastCommit("main")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "" {
		t.Fatalf("expected empty hash, got %q", hash)
	}

	// Set and retrieve
	if err := idx.SetLastCommit("main", "abc123"); err != nil {
		t.Fatal(err)
	}

	hash, err = idx.GetLastCommit("main")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "abc123" {
		t.Fatalf("expected 'abc123', got %q", hash)
	}

	// Overwrite
	if err := idx.SetLastCommit("main", "def456"); err != nil {
		t.Fatal(err)
	}

	hash, err = idx.GetLastCommit("main")
	if err != nil {
		t.Fatal(err)
	}
	if hash != "def456" {
		t.Fatalf("expected 'def456', got %q", hash)
	}
}

func TestIncrementalSync(t *testing.T) {
	// Use unified store.Service so git objects and index share one DB.
	dir := t.TempDir()
	svc, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	gitStore, err := git.InitWithStorer(svc.GitStorer(), nil, "")
	if err != nil {
		t.Fatal(err)
	}

	idx := svc.Index()

	// Write two fact files to the git store.
	fact1 := "---\ndomain: [databases]\nconfidence: 0.9\nsources: 2\nentities: [postgres]\nrefs: []\n---\n# Postgres MVCC\n\nPostgres uses multi-version concurrency control.\n"
	fact2 := "---\ndomain: [caching]\nconfidence: 0.8\nsources: 1\nentities: [redis]\nrefs: []\n---\n# Redis Persistence\n\nRedis supports AOF and RDB persistence.\n"

	if _, _, err := gitStore.WriteFile("kb/postgres-mvcc.md", fact1, "add postgres fact", "learn"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitStore.WriteFile("kb/redis-persistence.md", fact2, "add redis fact", "learn"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Sync(gitStore, gitStore.Branch()); err != nil {
		t.Fatalf("Sync (full rebuild) failed: %v", err)
	}

	// Both facts should now be retrievable.
	rec, err := idx.GetByPath("kb/postgres-mvcc.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected postgres fact after full sync")
	}

	rec, err = idx.GetByPath("kb/redis-persistence.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected redis fact after full sync")
	}

	// Verify last_commit was set.
	head, err := gitStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	last, err := idx.GetLastCommit(gitStore.Branch())
	if err != nil {
		t.Fatal(err)
	}
	if last != head {
		t.Fatalf("expected last_commit=%q, got %q", head, last)
	}

	// --- Incremental sync ---
	// Write a third fact. Sync should only index the delta.
	fact3 := "---\ndomain: [messaging]\nconfidence: 0.95\nsources: 3\nentities: [kafka]\nrefs: []\n---\n# Kafka Partitions\n\nKafka topics are split into partitions for parallelism.\n"
	if _, _, err := gitStore.WriteFile("kb/kafka-partitions.md", fact3, "add kafka fact", "learn"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Sync(gitStore, gitStore.Branch()); err != nil {
		t.Fatalf("Sync (incremental) failed: %v", err)
	}

	// New fact should be retrievable.
	rec, err = idx.GetByPath("kb/kafka-partitions.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected kafka fact after incremental sync")
	}

	// Previously indexed facts should still be present.
	rec, err = idx.GetByPath("kb/postgres-mvcc.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("expected postgres fact to survive incremental sync")
	}

	// --- Delete sync ---
	// Delete the redis fact and sync; it should be removed from the index.
	if _, err := gitStore.DeleteFile("kb/redis-persistence.md", "delete: remove redis fact", "retract"); err != nil {
		t.Fatal(err)
	}

	if err := idx.Sync(gitStore, gitStore.Branch()); err != nil {
		t.Fatalf("Sync (delete) failed: %v", err)
	}

	rec, err = idx.GetByPath("kb/redis-persistence.md")
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatal("expected redis fact to be removed after delete sync")
	}

	// No-op sync: calling Sync again with same HEAD should be a no-op.
	headAfter, err := gitStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.Sync(gitStore, gitStore.Branch()); err != nil {
		t.Fatalf("Sync (no-op) failed: %v", err)
	}
	lastAfter, err := idx.GetLastCommit(gitStore.Branch())
	if err != nil {
		t.Fatal(err)
	}
	if lastAfter != headAfter {
		t.Fatalf("no-op sync changed last_commit unexpectedly")
	}
}

// Regression test: commit_hash in the index should be the commit that last
// touched the file, not the HEAD commit at sync time.
func TestSyncCommitHashIsLastTouch(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(dir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	gitStore, err := git.InitWithStorer(svc.GitStorer(), nil, "")
	if err != nil {
		t.Fatal(err)
	}

	idx := svc.Index()

	fact1 := "---\ndomain: [a]\nconfidence: 0.9\nsources: 1\nentities: []\nrefs: []\n---\n# Fact A\n\nBody A.\n"
	fact2 := "---\ndomain: [b]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# Fact B\n\nBody B.\n"

	// Commit fact A first.
	commitA, _, err := gitStore.WriteFile("kb/a.md", fact1, "add A", "learn")
	if err != nil {
		t.Fatal(err)
	}

	// Commit fact B second — this becomes HEAD.
	_, _, err = gitStore.WriteFile("kb/b.md", fact2, "add B", "learn")
	if err != nil {
		t.Fatal(err)
	}

	head, err := gitStore.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}
	if commitA == head {
		t.Fatal("expected two distinct commits")
	}

	// Full rebuild sync.
	if err := idx.Sync(gitStore, gitStore.Branch()); err != nil {
		t.Fatal(err)
	}

	recA, err := idx.GetByPath("kb/a.md")
	if err != nil {
		t.Fatal(err)
	}
	if recA == nil {
		t.Fatal("expected fact A")
	}
	if recA.CommitHash == head {
		t.Fatalf("fact A commit_hash should be %q (its own commit), not HEAD %q", commitA, head)
	}
	if recA.CommitHash != commitA {
		t.Fatalf("fact A commit_hash = %q, want %q", recA.CommitHash, commitA)
	}

	// Now modify only fact A — after incremental sync, B should keep its original commit.
	recB, err := idx.GetByPath("kb/b.md")
	if err != nil {
		t.Fatal(err)
	}
	commitBBefore := recB.CommitHash

	fact1v2 := "---\ndomain: [a]\nconfidence: 0.95\nsources: 2\nentities: []\nrefs: []\n---\n# Fact A v2\n\nUpdated body.\n"
	commitA2, _, err := gitStore.WriteFile("kb/a.md", fact1v2, "update A", "learn")
	if err != nil {
		t.Fatal(err)
	}

	if err := idx.Sync(gitStore, gitStore.Branch()); err != nil {
		t.Fatal(err)
	}

	recA, _ = idx.GetByPath("kb/a.md")
	if recA.CommitHash != commitA2 {
		t.Fatalf("after update, fact A commit_hash = %q, want %q", recA.CommitHash, commitA2)
	}

	recB, _ = idx.GetByPath("kb/b.md")
	if recB.CommitHash != commitBBefore {
		t.Fatalf("fact B commit_hash changed to %q after unrelated sync, want %q", recB.CommitHash, commitBBefore)
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

	insertTestBlob(t, idx.DB(), "blob_emb", "body text for embedding")

	rec := store.FactRecord{
		Path:       "kb/test/emb.md",
		Title:      "Embedding test",
		BlobHash:   "blob_emb",
		Domain:     []string{"test"},
		Entities:   []string{},
		Confidence: 1.0,
		Sources:    1,
		CommitHash: "emb1",
	}
	if err := idx.Upsert(rec); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := idx.GetEmbedding("kb/test/emb.md")
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

// ── Search tests ──────────────────────────────────────────────────────────────

func TestSearchFilter(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "blob_a", "postgres database replication")
	insertTestBlob(t, idx.DB(), "blob_b", "redis cache cluster")

	if err := idx.Upsert(store.FactRecord{
		Path: "kb/a.md", Title: "Alpha", BlobHash: "blob_a",
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/b.md", Title: "Beta", BlobHash: "blob_b",
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
	if results[0].Path != "kb/a.md" {
		t.Fatalf("expected kb/a.md, got %v", results[0].Path)
	}

	// Text-less search filtered by entity.
	results, err = idx.Search(store.SearchQuery{Entities: []string{"redis"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for entity filter, got %d", len(results))
	}
	if results[0].Path != "kb/b.md" {
		t.Fatalf("expected kb/b.md, got %v", results[0].Path)
	}

	// Path filter should return only the fact whose path starts with "kb/a".
	results, err = idx.Search(store.SearchQuery{Path: "kb/a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for path filter, got %d", len(results))
	}
	if results[0].Path != "kb/a.md" {
		t.Fatalf("expected kb/a.md, got %v", results[0].Path)
	}

	// MinConfidence filter should drop low-confidence records.
	results, err = idx.Search(store.SearchQuery{MinConfidence: 0.85, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for confidence filter, got %d", len(results))
	}
	if results[0].Path != "kb/a.md" {
		t.Fatalf("expected kb/a.md, got %v", results[0].Path)
	}
}

func TestSearchHybrid(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	const dims = 4 // tiny dimension for test speed

	// Fact A: embedding points toward [1,0,0,0].
	vecA := []float32{1, 0, 0, 0}
	// Fact B: embedding is related but less similar.
	vecB := []float32{0.7, 0.7, 0, 0}

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

	insertTestBlob(t, idx.DB(), "blob_ha", "postgres database replication")
	insertTestBlob(t, idx.DB(), "blob_hb", "postgres cache storage")

	if err := idx.Upsert(store.FactRecord{
		Path: "kb/a.md", Title: "Alpha", BlobHash: "blob_ha",
		Domain: []string{"databases"}, Entities: []string{"postgres"},
		Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/b.md", Title: "Beta", BlobHash: "blob_hb",
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
		t.Fatal("expected results from vector search")
	}
	// Both facts should be returned.
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Fact A should rank first because its vector exactly matches the query vector.
	if results[0].Path != "kb/a.md" {
		t.Fatalf("expected kb/a.md first, got %v", results[0].Path)
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

	insertTestBlob(t, idx.DB(), "blob_ri", "referential integrity")

	rec := store.FactRecord{
		Path: "kb/test/ri.md", Title: "RI Test", BlobHash: "blob_ri",
		Domain: []string{"test"}, Entities: []string{},
		Confidence: 1.0, Sources: 1, CommitHash: "ri1",
	}
	if err := idx.Upsert(rec); err != nil {
		t.Fatal(err)
	}

	// Verify embedding exists.
	vec, err := idx.GetEmbedding("kb/test/ri.md")
	if err != nil {
		t.Fatal(err)
	}
	if vec == nil {
		t.Fatal("expected embedding after upsert")
	}

	// Delete the fact.
	if err := idx.Delete("kb/test/ri.md"); err != nil {
		t.Fatal(err)
	}

	// Embedding must be gone.
	vec, err = idx.GetEmbedding("kb/test/ri.md")
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

	insertTestBlob(t, idx.DB(), "blob_tea", "Carol drinks green tea exclusively")
	insertTestBlob(t, idx.DB(), "blob_jazz", "Bob listens to jazz regularly")
	insertTestBlob(t, idx.DB(), "blob_python", "Alice writes Python every day")

	facts := []store.FactRecord{
		{Path: "kb/people/carol/tea.md", Title: "Tea Preference", BlobHash: "blob_tea",
			Domain: []string{"preferences"}, Entities: []string{"carol"}, Confidence: 0.9, Sources: 1, CommitHash: "a"},
		{Path: "kb/people/bob/jazz.md", Title: "Jazz Fan", BlobHash: "blob_jazz",
			Domain: []string{"preferences"}, Entities: []string{"bob"}, Confidence: 0.8, Sources: 1, CommitHash: "a"},
		{Path: "kb/people/alice/python.md", Title: "Python Dev", BlobHash: "blob_python",
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
	if results[0].Path != "kb/people/carol/tea.md" {
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

func TestSearchVecOnlyNoEmbedder(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "blob_tea_ne", "tea drinking habits")

	// Insert a fact without embedder.
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/a.md", Title: "Tea Lover", BlobHash: "blob_tea_ne",
		Domain: []string{"pref"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	// Without embedder: text search returns nil (no vec hits).
	results, err := idx.Search(store.SearchQuery{Text: "tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results without embedder, got %d", len(results))
	}
}

func TestSearchVecScoringBoost(t *testing.T) {
	idx, err := store.New(":memory:", store.WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Two facts with embeddings at different cosine distances.
	vecs := map[string][]float32{
		"tea brewing techniques": {1, 0, 0, 0},
		"tea garden cultivation": {0.95, 0.05, 0, 0}, // slightly less similar
		"tea":                    {1, 0, 0, 0},        // query
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

	insertTestBlob(t, idx.DB(), "blob_brew", "tea brewing techniques")
	insertTestBlob(t, idx.DB(), "blob_garden", "tea garden cultivation")

	if err := idx.Upsert(store.FactRecord{
		Path: "kb/a.md", Title: "Brewing", BlobHash: "blob_brew",
		Domain: []string{"food"}, Entities: []string{}, Confidence: 0.9, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/b.md", Title: "Garden", BlobHash: "blob_garden",
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
	// Fact A (exact cosine match) should score higher than B (slightly lower cosine).
	if results[0].Score <= results[1].Score {
		t.Fatalf("expected results[0] > results[1], got %v <= %v", results[0].Score, results[1].Score)
	}
}

// ── Stats tests ───────────────────────────────────────────────────────────────

func TestStats_Empty(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	res, err := idx.Stats("")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 {
		t.Errorf("empty index: total = %d, want 0", res.Total)
	}
	if res.AvgConfidence != 0 {
		t.Errorf("empty index: avg_confidence = %v, want 0", res.AvgConfidence)
	}
}

func TestStats_Aggregate(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "b1", "body1")
	insertTestBlob(t, idx.DB(), "b2", "body2")
	insertTestBlob(t, idx.DB(), "b3", "body3")

	facts := []store.FactRecord{
		{Path: "kb/a.md", Title: "A", BlobHash: "b1", Domain: []string{"go", "web"}, Entities: []string{"chi"}, Confidence: 0.9, Sources: 1, CommitHash: "x"},
		{Path: "kb/b.md", Title: "B", BlobHash: "b2", Domain: []string{"go"}, Entities: []string{"chi", "mux"}, Confidence: 0.7, Sources: 1, CommitHash: "x"},
		{Path: "other/c.md", Title: "C", BlobHash: "b3", Domain: []string{"infra"}, Entities: []string{"k8s"}, Confidence: 1.0, Sources: 1, CommitHash: "x"},
	}
	for _, f := range facts {
		if err := idx.Upsert(f); err != nil {
			t.Fatal(err)
		}
	}

	// All facts (no prefix filter).
	res, err := idx.Stats("")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 {
		t.Errorf("total = %d, want 3", res.Total)
	}
	if res.Domains["go"] != 2 {
		t.Errorf("domains[go] = %d, want 2", res.Domains["go"])
	}
	if res.Domains["infra"] != 1 {
		t.Errorf("domains[infra] = %d, want 1", res.Domains["infra"])
	}
	if res.Entities["chi"] != 2 {
		t.Errorf("entities[chi] = %d, want 2", res.Entities["chi"])
	}

	// Prefix-filtered: only kb/ facts.
	res, err = idx.Stats("kb/")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 {
		t.Errorf("prefix filter: total = %d, want 2", res.Total)
	}
	if _, ok := res.Domains["infra"]; ok {
		t.Error("prefix filter: infra domain should not appear for kb/ prefix")
	}
	// avg_confidence for kb/ = (0.9 + 0.7) / 2 = 0.8
	if res.AvgConfidence != 0.8 {
		t.Errorf("prefix filter: avg_confidence = %v, want 0.8", res.AvgConfidence)
	}
}

func TestStats_NullDomainAndEntities(t *testing.T) {
	idx, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertTestBlob(t, idx.DB(), "b1", "body1")

	// Insert a fact with nil domain and entities (simulates missing frontmatter fields).
	if err := idx.Upsert(store.FactRecord{
		Path: "kb/bare.md", Title: "Bare", BlobHash: "b1",
		Domain: nil, Entities: nil,
		Confidence: 0.5, Sources: 1, CommitHash: "x",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := idx.Stats("")
	if err != nil {
		t.Fatalf("Stats with NULL domain/entities should not error: %v", err)
	}
	if res.Total != 1 {
		t.Errorf("total = %d, want 1", res.Total)
	}
}
