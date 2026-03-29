// Package store — GraphQLite self-loop bug repro.
//
// Bug: when MATCH (n:Label {key: "A"}), (s:Label {key: "B"}) is executed and
// "B" does not exist as a node, GraphQLite's MERGE creates a self-loop
// (A)-[:REL]->(A) instead of doing nothing.
//
// This affects ALL tested versions: 0.3.7, 0.3.8, 0.3.9, 0.3.10.
// None of the attempted Cypher guard clauses prevent it:
//   - WHERE n <> s
//   - WHERE n.path <> s.path
//   - MATCH (s) WITH s MATCH (n) MERGE (n)-[:REL]->(s)
//
// Upstream issue: https://github.com/colliery-io/graphqlite/issues/XXX
package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// openGraphQLiteDB opens a raw SQLite connection with GraphQLite loaded,
// initialising the EAV schema required for Cypher queries.
func openGraphQLiteDB(t *testing.T) *sql.DB {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	libPath := filepath.Join(repoRoot, "dist", "lib", "graphqlite")

	driverName := "sqlite3_graphqlite_repro_" + t.Name()
	sql.Register(driverName, &sqlite3.SQLiteDriver{
		Extensions: []string{libPath},
	})

	db, err := sql.Open(driverName, ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)

	// Initialise GraphQLite EAV schema.
	_, err = db.Exec(`SELECT cypher('RETURN 1')`)
	if err != nil {
		t.Skipf("GraphQLite not available (%v) — skipping", err)
	}

	t.Cleanup(func() { db.Close() })
	return db
}

// edgesFrom returns the paths of all outgoing DERIVED_FROM neighbours of src.
func edgesFrom(t *testing.T, db *sql.DB, src string) []string {
	t.Helper()
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.path') FROM json_each(cypher('MATCH (f:Fact {path: "%s"})-[:DERIVED_FROM]->(t) RETURN t.path AS path'))`,
		src,
	)
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("edgesFrom query: %v", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// TestGraphQLiteSelfLoopBug demonstrates the bug: executing a MATCH+MERGE
// where the target node does not exist creates a self-loop on the source node.
//
// Expected (correct): zero DERIVED_FROM edges — target doesn't exist, do nothing.
// Actual (bug):       one edge (source)-[:DERIVED_FROM]->(source) — self-loop.
func TestGraphQLiteSelfLoopBug(t *testing.T) {
	t.Skip("known upstream GraphQLite bug — tracked for future fix")
	db := openGraphQLiteDB(t)

	// Create only the source node; "kb/target.md" deliberately does NOT exist.
	if _, err := db.Exec(`SELECT cypher('MERGE (:Fact {path: "kb/source.md"})')`); err != nil {
		t.Fatalf("merge source: %v", err)
	}

	// Attempt to create a DERIVED_FROM edge to a non-existent target.
	db.Exec(`SELECT cypher('MATCH (n:Fact {path: "kb/source.md"}), (s:Fact {path: "kb/target.md"}) MERGE (n)-[:DERIVED_FROM]->(s)')`)

	edges := edgesFrom(t, db, "kb/source.md")
	if len(edges) != 0 {
		t.Errorf("BUG REPRODUCED: expected 0 edges (target absent), got %d: %v", len(edges), edges)
	}
}

// TestGraphQLiteSelfLoopBug_GuardClauses verifies that none of the Cypher-level
// guard approaches prevent the self-loop.
func TestGraphQLiteSelfLoopBug_GuardClauses(t *testing.T) {
	t.Skip("known upstream GraphQLite bug — tracked for future fix")
	guards := []struct {
		name   string
		cypher string
	}{
		{
			name:   "WHERE n <> s",
			cypher: `MATCH (n:Fact {path: "kb/source.md"}), (s:Fact {path: "kb/target.md"}) WHERE n <> s MERGE (n)-[:DERIVED_FROM]->(s)`,
		},
		{
			name:   "WHERE n.path <> s.path",
			cypher: `MATCH (n:Fact {path: "kb/source.md"}), (s:Fact {path: "kb/target.md"}) WHERE n.path <> s.path MERGE (n)-[:DERIVED_FROM]->(s)`,
		},
		{
			name:   "MATCH s WITH s MATCH n",
			cypher: `MATCH (s:Fact {path: "kb/target.md"}) WITH s MATCH (n:Fact {path: "kb/source.md"}) MERGE (n)-[:DERIVED_FROM]->(s)`,
		},
	}

	for _, g := range guards {
		t.Run(g.name, func(t *testing.T) {
			db := openGraphQLiteDB(t)

			// Only source exists.
			if _, err := db.Exec(`SELECT cypher('MERGE (:Fact {path: "kb/source.md"})')`); err != nil {
				t.Fatalf("merge source: %v", err)
			}

			db.Exec(fmt.Sprintf(`SELECT cypher('%s')`, g.cypher))

			edges := edgesFrom(t, db, "kb/source.md")
			if len(edges) != 0 {
				t.Errorf("guard %q does NOT prevent self-loop: got edges %v", g.name, edges)
			}
		})
	}
}

// TestGraphQLiteSelfLoopBug_GoodCase confirms the happy path still works:
// when the target node exists the edge is created correctly.
func TestGraphQLiteSelfLoopBug_GoodCase(t *testing.T) {
	db := openGraphQLiteDB(t)

	for _, p := range []string{"kb/source.md", "kb/target.md"} {
		if _, err := db.Exec(fmt.Sprintf(`SELECT cypher('MERGE (:Fact {path: "%s"})')`, p)); err != nil {
			t.Fatalf("merge %s: %v", p, err)
		}
	}

	if _, err := db.Exec(`SELECT cypher('MATCH (n:Fact {path: "kb/source.md"}), (s:Fact {path: "kb/target.md"}) MERGE (n)-[:DERIVED_FROM]->(s)')`); err != nil {
		t.Fatalf("merge edge: %v", err)
	}

	edges := edgesFrom(t, db, "kb/source.md")
	if len(edges) != 1 || edges[0] != "kb/target.md" {
		t.Errorf("expected [kb/target.md], got %v", edges)
	}
}
