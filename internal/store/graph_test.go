package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// testVec creates a 768-dimensional vector padded with zeros. The provided values
// set the leading components, preserving relative similarity between test vectors.
func testVec(values ...float32) []float32 {
	v := make([]float32, 768)
	copy(v, values)
	return v
}

// insertBlob inserts a fake blob into the objects table for testing.
// content is the full raw file content (frontmatter + body).
// If content looks like a plain body (no frontmatter), it is wrapped.
func insertBlob(t *testing.T, db *sql.DB, hash, content string) {
	t.Helper()
	if !strings.HasPrefix(content, "---\n") {
		content = "---\ndomain: [test]\nconfidence: 0.9\nsources: 1\n---\n# Title\n\n" + content
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO objects(hash, type, size, data) VALUES (?, ?, ?, ?)`,
		hash, BlobObjectType, len(content), []byte(content))
	if err != nil {
		t.Fatalf("insertBlob %s: %v", hash, err)
	}
}

// stubEmbedder768d returns deterministic 768-dimensional embeddings based on the text.
type stubEmbedder768d struct{}

func (s *stubEmbedder768d) Embed(text string) ([]float32, error) {
	// Return similar vectors for different inputs so KNN finds neighbors
	switch text {
	case "alpha":
		return testVec(1.0, 0.1, 0.0, 0.0), nil
	case "beta":
		return testVec(0.9, 0.2, 0.0, 0.0), nil
	case "gamma":
		return testVec(0.0, 0.0, 1.0, 0.1), nil
	default:
		return testVec(0.5, 0.5, 0.5, 0.5), nil
	}
}

// graphqliteTestPath returns the absolute path to the vendored GraphQLite
// shared library for the current platform, without the file extension.
// The mattn/go-sqlite3 driver appends the platform extension automatically.
func graphqliteTestPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	return filepath.Join(repoRoot, "dist", "lib", "graphqlite")
}

func TestNew_InitializesGraphQLite(t *testing.T) {
	// Verifies that New() applies all migrations including GraphQLite init,
	// which creates the nodes and edges EAV tables.
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	for _, table := range []string{"nodes", "edges"} {
		var name string
		err = idx.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("expected GraphQLite table %q to exist after New(): %v", table, err)
		}
	}
}

func TestNewWithGraphQLite(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Verify cypher() is available on the Index's db connection.
	var result string
	err = idx.db.QueryRow(`SELECT cypher('RETURN 1')`).Scan(&result)
	if err != nil {
		t.Fatalf("cypher not available on Index db: %v", err)
	}

	// Verify vec0 still works.
	var d float64
	err = idx.db.QueryRow(`SELECT vec_distance_cosine(vec_f32('[1,0]'), vec_f32('[0,1]'))`).Scan(&d)
	if err != nil {
		t.Fatalf("vec_distance_cosine failed: %v", err)
	}
}

func TestGraphMergeFact(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	err = idx.graphSyncFact(FactRecord{
		Path:     "kb/test/fact.md",
		Title:    "Test Fact",
		Domain:   []string{"engineering/software"},
		Entities: []string{"Go", "SQLite"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify Fact node exists
	var path string
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact {path: "kb/test/fact.md"}) RETURN f.path AS path'))`).Scan(&path)
	if err != nil {
		t.Fatalf("Fact node not found: %v", err)
	}
	if path != "kb/test/fact.md" {
		t.Fatalf("expected path kb/test/fact.md, got %q", path)
	}
}

func TestGraphMergeFactWithApostrophe(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Regression: apostrophe in title broke the outer SQL string in
	// SELECT cypher('...'), producing: near "s": syntax error
	err = idx.graphSyncFact(FactRecord{
		Path:     "kb/people/dave/postgres-expert.md",
		Title:    "Dave's Postgres expertise",
		Domain:   []string{"engineering"},
		Entities: []string{"Dave", "PostgreSQL"},
	})
	if err != nil {
		t.Fatalf("graphSyncFact with apostrophe: %v", err)
	}

	var title string
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.title') FROM json_each(cypher('MATCH (f:Fact {path: "kb/people/dave/postgres-expert.md"}) RETURN f.title AS title'))`).Scan(&title)
	if err != nil {
		t.Fatalf("Fact node not found: %v", err)
	}
	if title != "Dave's Postgres expertise" {
		t.Fatalf("expected title with apostrophe, got %q", title)
	}
}

func TestGraphDomainHierarchy(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	err = idx.graphSyncFact(FactRecord{
		Path:   "kb/test/fact.md",
		Domain: []string{"engineering/software/applications/web-server"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify domain ancestor chain was created
	rows, err := idx.db.Query(`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (d:Domain) RETURN d.path AS path'))`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	domains := map[string]bool{}
	for rows.Next() {
		var d string
		rows.Scan(&d)
		domains[d] = true
	}

	expected := []string{
		"engineering",
		"engineering/software",
		"engineering/software/applications",
		"engineering/software/applications/web-server",
	}
	for _, e := range expected {
		if !domains[e] {
			t.Errorf("missing domain node: %s", e)
		}
	}
}

func TestGraphDeleteFact(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Create then delete
	_ = idx.graphSyncFact(FactRecord{
		Path:     "kb/test/fact.md",
		Domain:   []string{"eng"},
		Entities: []string{"Go"},
	})
	err = idx.graphDeleteFact("kb/test/fact.md")
	if err != nil {
		t.Fatal(err)
	}

	// Fact node should be marked deleted.
	// json_extract returns integer 1 for JSON boolean true (SQLite has no bool type).
	var deleted int
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.deleted') FROM json_each(cypher('MATCH (f:Fact {path: "kb/test/fact.md"}) RETURN f.deleted AS deleted'))`).Scan(&deleted)
	if err != nil {
		t.Fatalf("Fact node not found after delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1, got %d", deleted)
	}
}

func TestGraphBuildSimilarityEdges(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	idx.SetEmbedder(&stubEmbedder768d{})
	insertBlob(t, idx.db, "hash_alpha", "alpha")
	insertBlob(t, idx.db, "hash_beta", "beta")
	facts := []FactRecord{
		{Path: "kb/a.md", Title: "A", BlobHash: "hash_alpha", Domain: []string{"test"}, Entities: []string{}, Refs: []string{}},
		{Path: "kb/b.md", Title: "B", BlobHash: "hash_beta", Domain: []string{"test"}, Entities: []string{}, Refs: []string{}},
	}
	for _, f := range facts {
		if err := idx.Upsert(testBranch, "abc", f); err != nil {
			t.Fatal(err)
		}
	}

	err = idx.graphBuildSimilarityEdges("kb/a.md")
	if err != nil {
		t.Fatal(err)
	}

	var count int
	err = idx.db.QueryRow(`SELECT count(*) FROM json_each(cypher('MATCH (:Fact {path: "kb/a.md"})-[:SIMILAR_TO]->(:Fact) RETURN 1 AS n'))`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("expected at least one SIMILAR_TO edge")
	}
}

func TestGraphQLiteCoexistence(t *testing.T) {
	// Register a custom driver that loads GraphQLite via Extensions.
	// sqlite-vec is loaded separately via Auto() (CGo bindings on default driver).
	registerVec()

	libPath := graphqliteTestPath(t)
	sql.Register("sqlite3_graphqlite_spike", &sqlite3.SQLiteDriver{
		Extensions: []string{libPath},
	})

	db, err := sql.Open("sqlite3_graphqlite_spike", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 1. Verify cypher() works
	var cypherResult string
	err = db.QueryRow(`SELECT cypher('RETURN 1')`).Scan(&cypherResult)
	if err != nil {
		t.Fatalf("cypher('RETURN 1') failed: %v", err)
	}
	t.Logf("cypher result: %s", cypherResult)

	// 2. Verify vec_distance_cosine still works (sqlite-vec loaded via Auto())
	var vecResult float64
	err = db.QueryRow(`SELECT vec_distance_cosine(vec_f32('[1,0,0,0]'), vec_f32('[0,1,0,0]'))`).Scan(&vecResult)
	if err != nil {
		t.Fatalf("vec_distance_cosine failed: %v", err)
	}
	t.Logf("vec distance: %f", vecResult)

	// 3. Verify GraphQLite EAV tables exist
	var tableName string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='nodes'`).Scan(&tableName)
	if err != nil {
		t.Fatalf("GraphQLite EAV tables not created: %v", err)
	}
	t.Logf("EAV table found: %s", tableName)

	// 4. Verify vec0 virtual table works alongside GraphQLite
	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS test_vec USING vec0(embedding FLOAT[4] distance_metric=cosine)`)
	if err != nil {
		t.Fatalf("vec0 virtual table creation failed: %v", err)
	}
	t.Log("vec0 virtual table created successfully alongside GraphQLite")
}

func TestUpsertSyncsGraph(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})
	insertBlob(t, idx.db, "hash_test", "test content")

	err = idx.Upsert(testBranch, "abc", FactRecord{
		Path: "kb/eng/test.md", Title: "Test", BlobHash: "hash_test",
		Domain: []string{"engineering/software"}, Entities: []string{"Go"},
		Refs: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fact node should exist in graph
	var factPath string
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact {path: "kb/eng/test.md"}) RETURN f.path AS path'))`).Scan(&factPath)
	if err != nil {
		t.Fatalf("Fact node not in graph after Upsert: %v", err)
	}

	// Entity node should exist
	var entityName string
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.name') FROM json_each(cypher('MATCH (e:Entity {name: "Go"}) RETURN e.name AS name'))`).Scan(&entityName)
	if err != nil {
		t.Fatalf("Entity node not in graph: %v", err)
	}
}

func TestDeleteSyncsGraph(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})
	insertBlob(t, idx.db, "hash_del", "delete test")

	_ = idx.Upsert(testBranch, "abc", FactRecord{
		Path: "kb/test.md", Title: "Test", BlobHash: "hash_del",
		Domain: []string{"eng"}, Entities: []string{"Go"},
		Refs: []string{},
	})
	err = idx.Delete(testBranch, "kb/test.md")
	if err != nil {
		t.Fatal(err)
	}

	// Fact should be marked deleted (json_extract returns 1 for JSON true)
	var deleted int
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.deleted') FROM json_each(cypher('MATCH (f:Fact {path: "kb/test.md"}) RETURN f.deleted AS deleted'))`).Scan(&deleted)
	if err != nil {
		t.Fatalf("Fact node missing after Delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1, got %d", deleted)
	}
}

func TestClusterFactsLouvain(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})
	insertBlob(t, idx.db, "hash_alpha", "alpha")
	insertBlob(t, idx.db, "hash_beta", "beta")
	insertBlob(t, idx.db, "hash_gamma", "gamma")

	// Create facts that share entities (will form a cluster via TAGGED edges)
	facts := []FactRecord{
		{Path: "kb/a.md", Title: "A", BlobHash: "hash_alpha", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}},
		{Path: "kb/b.md", Title: "B", BlobHash: "hash_beta", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}},
		{Path: "kb/c.md", Title: "C", BlobHash: "hash_gamma", Domain: []string{"eng"}, Entities: []string{"Go"}, Refs: []string{}},
	}
	for _, f := range facts {
		if err := idx.Upsert(testBranch, "abc", f); err != nil {
			t.Fatal(err)
		}
	}

	result, err := idx.ClusterFacts(testBranch, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	// Should have at least one cluster
	if len(result.Clusters) == 0 {
		t.Fatal("expected at least one cluster")
	}

	// All facts sharing entities should be in a community
	total := 0
	for _, members := range result.Clusters {
		total += len(members)
	}
	t.Logf("clusters: %d, total members: %d, noise: %d", len(result.Clusters), total, len(result.Noise))
}

func TestClusterFactsBranchScoped(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})

	insertBlob(t, idx.db, "hash_a", "alpha content")
	insertBlob(t, idx.db, "hash_b", "beta content")
	insertBlob(t, idx.db, "hash_c", "gamma content")
	insertBlob(t, idx.db, "hash_d", "delta content")

	branchA := "agent/branch-a"
	branchB := "agent/branch-b"

	// Facts on branchA share entities Go+SQLite → they cluster together.
	for _, f := range []FactRecord{
		{Path: "kb/a.md", Title: "A", BlobHash: "hash_a", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}},
		{Path: "kb/b.md", Title: "B", BlobHash: "hash_b", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}},
	} {
		if err := idx.Upsert(branchA, "commit1", f); err != nil {
			t.Fatal(err)
		}
	}

	// Facts on branchB share the same entities but live on a different branch.
	for _, f := range []FactRecord{
		{Path: "kb/c.md", Title: "C", BlobHash: "hash_c", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}},
		{Path: "kb/d.md", Title: "D", BlobHash: "hash_d", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}},
	} {
		if err := idx.Upsert(branchB, "commit2", f); err != nil {
			t.Fatal(err)
		}
	}

	// ClusterFacts scoped to branchA should only return branchA facts.
	result, err := idx.ClusterFacts(branchA, 1.0, 2)
	if err != nil {
		t.Fatal(err)
	}

	allPaths := make(map[string]bool)
	for _, members := range result.Clusters {
		for _, p := range members {
			allPaths[p] = true
		}
	}
	for _, p := range result.Noise {
		allPaths[p] = true
	}

	// branchA facts must be present
	if !allPaths["kb/a.md"] || !allPaths["kb/b.md"] {
		t.Fatalf("expected branchA facts in results, got paths: %v", allPaths)
	}
	// branchB facts must be absent
	if allPaths["kb/c.md"] || allPaths["kb/d.md"] {
		t.Fatalf("branchB facts should not appear in branchA clusters, got paths: %v", allPaths)
	}
}

func TestSearchWithGraphExpansion(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})
	insertBlob(t, idx.db, "hash_alpha", "alpha")
	insertBlob(t, idx.db, "hash_beta", "beta")

	_ = idx.Upsert(testBranch, "abc", FactRecord{
		Path: "kb/a.md", Title: "A", BlobHash: "hash_alpha",
		Domain: []string{"eng"}, Entities: []string{"Go"},
		Refs: []string{},
	})
	_ = idx.Upsert(testBranch, "abc", FactRecord{
		Path: "kb/b.md", Title: "B", BlobHash: "hash_beta",
		Domain: []string{"eng"}, Entities: []string{"Go"},
		Refs: []string{},
	})

	results, err := idx.Search(testBranch, SearchQuery{
		Text:      "alpha",
		GraphHops: 1,
		Limit:     10,
	})
	if err != nil {
		t.Fatal(err)
	}

	paths := map[string]bool{}
	for _, r := range results {
		paths[r.Path] = true
	}
	t.Logf("results: %v", paths)
	// With graph expansion, fact B should appear (connected via shared Entity "Go")
	if !paths["kb/b.md"] {
		t.Error("expected kb/b.md in results via graph expansion (shared Entity Go)")
	}
}

func TestGraphExpandSearch_MultiSeed(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})

	// alpha=[1,0.1,0,0], beta=[0.9,0.2,0,0] — highly similar (cosine > 0.60 threshold)
	// gamma=[0,0,1,0.1] — dissimilar from alpha/beta
	insertBlob(t, idx.db, "hash_alpha", "alpha")
	insertBlob(t, idx.db, "hash_beta", "beta")
	insertBlob(t, idx.db, "hash_gamma", "gamma")

	// seed1=alpha, seed2=gamma; fact3=beta is similar to seed1 via SIMILAR_TO.
	// With batch OR query, fact3 should be discovered from seed1.
	facts := []FactRecord{
		{Path: "kb/f1.md", Title: "F1", BlobHash: "hash_alpha", Domain: []string{"eng"}, Entities: []string{}, Refs: []string{}},
		{Path: "kb/f2.md", Title: "F2", BlobHash: "hash_gamma", Domain: []string{"eng"}, Entities: []string{}, Refs: []string{}},
		{Path: "kb/f3.md", Title: "F3", BlobHash: "hash_beta", Domain: []string{"eng"}, Entities: []string{}, Refs: []string{}},
	}
	for _, f := range facts {
		if err := idx.Upsert(testBranch, "abc", f); err != nil {
			t.Fatal(err)
		}
	}

	// Build SIMILAR_TO edges so that kb/f1.md ↔ kb/f3.md are connected.
	if err := idx.graphBuildSimilarityEdges("kb/f1.md"); err != nil {
		t.Fatal(err)
	}
	if err := idx.graphBuildSimilarityEdges("kb/f3.md"); err != nil {
		t.Fatal(err)
	}

	seeds := map[string]float64{
		"kb/f1.md": 0.9,  // alpha — similar to beta (f3)
		"kb/f2.md": 0.85, // gamma — dissimilar, no SIMILAR_TO neighbors
	}
	expanded := idx.graphExpandSearch(seeds, 1)

	// kb/f3.md should be discovered as a SIMILAR_TO neighbor of seed kb/f1.md
	if _, ok := expanded["kb/f3.md"]; !ok {
		t.Errorf("expected kb/f3.md to be discovered via SIMILAR_TO from seed kb/f1.md; got %v", expanded)
	}
	// Seeds must not appear in the expanded results
	if _, ok := expanded["kb/f1.md"]; ok {
		t.Error("seed kb/f1.md must not appear in expanded results")
	}
	if _, ok := expanded["kb/f2.md"]; ok {
		t.Error("seed kb/f2.md must not appear in expanded results")
	}
}

type mockGitReader struct {
	files       map[string]string
	blobHashes  map[string]string            // path → blob hash
	commitFiles map[string]map[string]string // commitHash → path → content
	head        string
}

func (m *mockGitReader) DiffFiles(branch, from string) (added, modified, deleted []string, err error) {
	return nil, nil, nil, nil
}
func (m *mockGitReader) ReadFile(branch, path string) (string, error) {
	if c, ok := m.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("not found: %s", path)
}
func (m *mockGitReader) ReadFileWithHash(branch, path string) (string, string, error) {
	c, ok := m.files[path]
	if !ok {
		return "", "", fmt.Errorf("not found: %s", path)
	}
	hash := "blob_" + path
	if m.blobHashes != nil {
		if h, ok := m.blobHashes[path]; ok {
			hash = h
		}
	}
	return c, hash, nil
}
func (m *mockGitReader) HeadCommit(branch string) (string, error) { return m.head, nil }
func (m *mockGitReader) LastCommitForPath(branch, path string) (string, error) {
	return m.head, nil // mock: return head as the last commit
}
func (m *mockGitReader) ReadFileAtCommit(branch, path, commitHash string) (string, error) {
	if m.commitFiles != nil {
		if byPath, ok := m.commitFiles[commitHash]; ok {
			if c, ok := byPath[path]; ok {
				return c, nil
			}
		}
	}
	// Fall back to current files
	if c, ok := m.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("ReadFileAtCommit: not found: %s@%s", path, commitHash)
}
func (m *mockGitReader) ListAll(branch string) ([]string, error) {
	paths, _, err := m.ListAllWithHash(branch)
	return paths, err
}
func (m *mockGitReader) ListAllWithHash(branch string) ([]string, []string, error) {
	var paths, hashes []string
	for p := range m.files {
		paths = append(paths, p)
		hash := "blob_" + p
		if m.blobHashes != nil {
			if h, ok := m.blobHashes[p]; ok {
				hash = h
			}
		}
		hashes = append(hashes, hash)
	}
	return paths, hashes, nil
}

func TestDerivedFromInvariant(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Upsert a fact with two local refs and one external URL.
	rec := FactRecord{
		Path:       "kb/a.md",
		Title:      "A",
		Domain:     []string{"test"},
		Refs:       []string{"kb/b.md", "kb/c.md", "https://example.com"},
	}
	// Upsert b and c first so the graph nodes exist.
	for _, path := range []string{"kb/b.md", "kb/c.md"} {
		if err := idx.Upsert(testBranch, "abc", FactRecord{Path: path, Title: path, Domain: []string{"test"}}); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
	}
	if err := idx.Upsert(testBranch, "abc", rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Query DERIVED_FROM edges for kb/a.md.
	got := derivedFromPaths(t, idx, "kb/a.md")
	want := map[string]bool{"kb/b.md": true, "kb/c.md": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DERIVED_FROM edges = %v, want %v", got, want)
	}

	// Re-upsert with changed refs: drop kb/c.md, add kb/d.md.
	if err := idx.Upsert(testBranch, "abc", FactRecord{Path: "kb/d.md", Title: "D", Domain: []string{"test"}}); err != nil {
		t.Fatalf("upsert d: %v", err)
	}
	rec.Refs = []string{"kb/b.md", "kb/d.md"}
	rec.BlobHash = "bh_a_v2" // Different blob hash to avoid COW shortcut
	if err := idx.Upsert(testBranch, "abc", rec); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got = derivedFromPaths(t, idx, "kb/a.md")
	want = map[string]bool{"kb/b.md": true, "kb/d.md": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after re-upsert DERIVED_FROM = %v, want %v", got, want)
	}
}

// derivedFromPaths returns the set of target paths for DERIVED_FROM edges from src.
func derivedFromPaths(t *testing.T, idx *Index, src string) map[string]bool {
	t.Helper()
	pj := jsonParams("path", src)
	rows, err := idx.db.Query(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact {path: $path})-[:DERIVED_FROM]->(t:Fact) RETURN t.path AS path', ?))`,
		pj,
	)
	if err != nil {
		t.Fatalf("query DERIVED_FROM: %v", err)
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var p string
		rows.Scan(&p)
		if p != "" {
			result[p] = true
		}
	}
	return result
}

func TestExplainFact(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	// Set up: a → b, a → c, d → a. All current (not deleted).
	// Upsert targets before referrers so edges resolve (no self-loops).
	facts := []FactRecord{
		{Path: "kb/b.md", Title: "Fact B", Domain: []string{"test"}},
		{Path: "kb/c.md", Title: "Fact C", Domain: []string{"test"}},
		{Path: "kb/a.md", Title: "Fact A", Domain: []string{"test"}, Refs: []string{"kb/b.md", "kb/c.md"}},
		{Path: "kb/d.md", Title: "Fact D", Domain: []string{"test"}, Refs: []string{"kb/a.md"}},
	}
	for _, f := range facts {
		if err := idx.Upsert(testBranch, "abc", f); err != nil {
			t.Fatalf("upsert %s: %v", f.Path, err)
		}
	}

	res, err := idx.ExplainFact("kb/a.md")
	if err != nil {
		t.Fatalf("ExplainFact: %v", err)
	}

	// Incoming: d references a.
	if len(res.Incoming) != 1 || res.Incoming[0].Path != "kb/d.md" || res.Incoming[0].Title != "Fact D" {
		t.Errorf("Incoming = %+v, want [{kb/d.md Fact D}]", res.Incoming)
	}
	// Outgoing: a references b and c, both not deleted.
	if len(res.Outgoing) != 2 {
		t.Errorf("Outgoing len = %d, want 2", len(res.Outgoing))
	}
	for _, r := range res.Outgoing {
		if r.Deleted {
			t.Errorf("outgoing %s unexpectedly marked deleted", r.Path)
		}
	}

	// Delete kb/c.md and re-explain: c should appear as deleted in outgoing.
	if err := idx.Delete(testBranch, "kb/c.md"); err != nil {
		t.Fatalf("delete c: %v", err)
	}
	res2, err := idx.ExplainFact("kb/a.md")
	if err != nil {
		t.Fatalf("ExplainFact after delete: %v", err)
	}
	deletedPaths := map[string]bool{}
	for _, r := range res2.Outgoing {
		if r.Deleted {
			deletedPaths[r.Path] = true
		}
	}
	if !deletedPaths["kb/c.md"] {
		t.Errorf("expected kb/c.md to be marked deleted, outgoing = %+v", res2.Outgoing)
	}
}

// TestExplainFact_SelfLoopFiltered verifies that ExplainFact strips self-loops.
// When a fact is indexed with a ref to a node that doesn't exist yet, GraphQLite
// creates a self-loop (f)-[:DERIVED_FROM]->(f). ExplainFact must not expose it.
func TestExplainFact_SelfLoopFiltered(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer idx.Close()

	// Index fact A with a ref to a non-existent target — causes a self-loop.
	if err := idx.Upsert(testBranch, "abc", FactRecord{
		Path:       "kb/a.md",
		Title:      "Fact A",
		Domain:     []string{"test"},
		Refs:       []string{"kb/missing.md"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	res, err := idx.ExplainFact("kb/a.md")
	if err != nil {
		t.Fatalf("ExplainFact: %v", err)
	}
	for _, r := range res.Outgoing {
		if r.Path == "kb/a.md" {
			t.Errorf("self-loop leaked into Outgoing: %+v", r)
		}
	}
	for _, r := range res.Incoming {
		if r.Path == "kb/a.md" {
			t.Errorf("self-loop leaked into Incoming: %+v", r)
		}
	}
}

func TestSyncRebuildsGraph(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder768d{})

	contentA := "---\ndomain: [eng]\nentities: [Go]\nconfidence: 0.9\nsources: 1\n---\n# A\n\nBody A"
	contentB := "---\ndomain: [eng]\nentities: [Rust]\nconfidence: 0.8\nsources: 1\n---\n# B\n\nBody B"
	insertBlob(t, idx.db, "blob_kb/a.md", contentA)
	insertBlob(t, idx.db, "blob_kb/b.md", contentB)

	git := &mockGitReader{
		files: map[string]string{
			"kb/a.md": contentA,
			"kb/b.md": contentB,
		},
		head: "abc123def456",
	}

	err = idx.Sync(git, "main")
	if err != nil {
		t.Fatal(err)
	}

	// Verify graph has fact nodes
	var factCount int
	err = idx.db.QueryRow(`SELECT count(*) FROM json_each(cypher('MATCH (f:Fact) RETURN f.path AS path'))`).Scan(&factCount)
	if err != nil {
		t.Fatal(err)
	}
	if factCount < 2 {
		t.Fatalf("expected at least 2 fact nodes, got %d", factCount)
	}
}
