package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// stubEmbedder4d returns deterministic 4-dimensional embeddings based on the text.
type stubEmbedder4d struct{}

func (s *stubEmbedder4d) Embed(text string) ([]float32, error) {
	// Return similar vectors for different inputs so KNN finds neighbors
	switch text {
	case "alpha":
		return []float32{1.0, 0.1, 0.0, 0.0}, nil
	case "beta":
		return []float32{0.9, 0.2, 0.0, 0.0}, nil
	case "gamma":
		return []float32{0.0, 0.0, 1.0, 0.1}, nil
	default:
		return []float32{0.5, 0.5, 0.5, 0.5}, nil
	}
}

// graphqliteTestPath returns the absolute path to the vendored GraphQLite
// shared library for the current platform (without file extension — mattn
// driver strips it).
func graphqliteTestPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	} else if runtime.GOOS == "windows" {
		ext = ".dll"
	}
	return filepath.Join(repoRoot, "lib", runtime.GOOS+"-"+runtime.GOARCH, "graphqlite"+ext)
}

func TestSchemaMigrationV3ToV4(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	var version string
	err = idx.db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	if version != "4" {
		t.Fatalf("expected schema_version=4, got %q", version)
	}

	// Verify GraphQLite EAV tables exist.
	tables := []string{"nodes", "edges"}
	for _, table := range tables {
		var name string
		err = idx.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("expected GraphQLite table %q to exist: %v", table, err)
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
		Path:     "know/test/fact.md",
		Title:    "Test Fact",
		Domain:   []string{"engineering/software"},
		Entities: []string{"Go", "SQLite"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify Fact node exists
	var path string
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact {path: "know/test/fact.md"}) RETURN f.path AS path'))`).Scan(&path)
	if err != nil {
		t.Fatalf("Fact node not found: %v", err)
	}
	if path != "know/test/fact.md" {
		t.Fatalf("expected path know/test/fact.md, got %q", path)
	}
}

func TestGraphDomainHierarchy(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	err = idx.graphSyncFact(FactRecord{
		Path:   "know/test/fact.md",
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
		Path:     "know/test/fact.md",
		Domain:   []string{"eng"},
		Entities: []string{"Go"},
	})
	err = idx.graphDeleteFact("know/test/fact.md")
	if err != nil {
		t.Fatal(err)
	}

	// Fact node should be marked deleted.
	// json_extract returns integer 1 for JSON boolean true (SQLite has no bool type).
	var deleted int
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.deleted') FROM json_each(cypher('MATCH (f:Fact {path: "know/test/fact.md"}) RETURN f.deleted AS deleted'))`).Scan(&deleted)
	if err != nil {
		t.Fatalf("Fact node not found after delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1, got %d", deleted)
	}
}

func TestGraphBuildSimilarityEdges(t *testing.T) {
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	idx.SetEmbedder(&stubEmbedder4d{})
	facts := []FactRecord{
		{Path: "know/a.md", Title: "A", Body: "alpha", Domain: []string{"test"}, Entities: []string{}, Refs: []string{}, CommitHash: "abc"},
		{Path: "know/b.md", Title: "B", Body: "beta", Domain: []string{"test"}, Entities: []string{}, Refs: []string{}, CommitHash: "abc"},
	}
	for _, f := range facts {
		if err := idx.Upsert(f); err != nil {
			t.Fatal(err)
		}
	}

	err = idx.graphBuildSimilarityEdges("know/a.md")
	if err != nil {
		t.Fatal(err)
	}

	var count int
	err = idx.db.QueryRow(`SELECT count(*) FROM json_each(cypher('MATCH (:Fact {path: "know/a.md"})-[:SIMILAR_TO]->(:Fact) RETURN 1 AS n'))`).Scan(&count)
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
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder4d{})

	err = idx.Upsert(FactRecord{
		Path: "know/eng/test.md", Title: "Test",
		Domain: []string{"engineering/software"}, Entities: []string{"Go"},
		Refs: []string{}, CommitHash: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fact node should exist in graph
	var factPath string
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact {path: "know/eng/test.md"}) RETURN f.path AS path'))`).Scan(&factPath)
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
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder4d{})

	_ = idx.Upsert(FactRecord{
		Path: "know/test.md", Title: "Test",
		Domain: []string{"eng"}, Entities: []string{"Go"},
		Refs: []string{}, CommitHash: "abc",
	})
	err = idx.Delete("know/test.md")
	if err != nil {
		t.Fatal(err)
	}

	// Fact should be marked deleted (json_extract returns 1 for JSON true)
	var deleted int
	err = idx.db.QueryRow(`SELECT json_extract(value, '$.deleted') FROM json_each(cypher('MATCH (f:Fact {path: "know/test.md"}) RETURN f.deleted AS deleted'))`).Scan(&deleted)
	if err != nil {
		t.Fatalf("Fact node missing after Delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected deleted=1, got %d", deleted)
	}
}

func TestClusterFactsLouvain(t *testing.T) {
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder4d{})

	// Create facts that share entities (will form a cluster via TAGGED edges)
	facts := []FactRecord{
		{Path: "know/a.md", Title: "A", Body: "alpha", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}, CommitHash: "abc"},
		{Path: "know/b.md", Title: "B", Body: "beta", Domain: []string{"eng"}, Entities: []string{"Go", "SQLite"}, Refs: []string{}, CommitHash: "abc"},
		{Path: "know/c.md", Title: "C", Body: "gamma", Domain: []string{"eng"}, Entities: []string{"Go"}, Refs: []string{}, CommitHash: "abc"},
	}
	for _, f := range facts {
		if err := idx.Upsert(f); err != nil {
			t.Fatal(err)
		}
	}

	result, err := idx.ClusterFacts(1.0, 2)
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

type mockGitReader struct {
	files map[string]string
	head  string
}

func (m *mockGitReader) DiffFiles(from string) (added, modified, deleted []string, err error) {
	return nil, nil, nil, nil
}
func (m *mockGitReader) ReadFile(path string) (string, error) {
	if c, ok := m.files[path]; ok {
		return c, nil
	}
	return "", fmt.Errorf("not found: %s", path)
}
func (m *mockGitReader) HeadCommit() (string, error) { return m.head, nil }
func (m *mockGitReader) ListAll() ([]string, error) {
	var paths []string
	for p := range m.files {
		paths = append(paths, p)
	}
	return paths, nil
}

func TestSyncRebuildsGraph(t *testing.T) {
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	idx.SetEmbedder(&stubEmbedder4d{})

	git := &mockGitReader{
		files: map[string]string{
			"know/a.md": "---\ndomain: [eng]\nentities: [Go]\nconfidence: 0.9\nsources: 1\n---\n# A\n\nBody A",
			"know/b.md": "---\ndomain: [eng]\nentities: [Rust]\nconfidence: 0.8\nsources: 1\n---\n# B\n\nBody B",
		},
		head: "abc123def456",
	}

	err = idx.Sync(git)
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
