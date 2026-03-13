package store

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

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
