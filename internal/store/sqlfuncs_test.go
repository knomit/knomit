package store

import (
	"database/sql"
	"encoding/json"
	"math"
	"testing"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	registerVec() // ensures sqlite3_knomit driver is registered
	db, err := sql.Open("sqlite3_knomit", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLParseFact_ValidFact(t *testing.T) {
	db := openTestDB(t)

	blob := "---\ntype: principle\ndomain: [databases, sql]\nentities: [postgres, mysql]\nconfidence: 0.9\nsources: 2\nrefs: [https://example.com]\n---\n# My Title\n\nBody content."

	var result sql.NullString
	err := db.QueryRow("SELECT knomit_parse_fact(?)", []byte(blob)).Scan(&result)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !result.Valid {
		t.Fatal("expected non-NULL result")
	}

	var pf parsedFact
	if err := json.Unmarshal([]byte(result.String), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if pf.Title != "My Title" {
		t.Errorf("title = %q, want %q", pf.Title, "My Title")
	}
	if pf.Type != "principle" {
		t.Errorf("type = %q, want %q", pf.Type, "principle")
	}
	if len(pf.Domain) != 2 || pf.Domain[0] != "databases" || pf.Domain[1] != "sql" {
		t.Errorf("domain = %v, want [databases sql]", pf.Domain)
	}
	if len(pf.Entities) != 2 || pf.Entities[0] != "postgres" {
		t.Errorf("entities = %v, want [postgres mysql]", pf.Entities)
	}
	if pf.Confidence != 0.9 {
		t.Errorf("confidence = %f, want 0.9", pf.Confidence)
	}
	if pf.Sources != 2 {
		t.Errorf("sources = %d, want 2", pf.Sources)
	}
	if len(pf.Refs) != 1 || pf.Refs[0] != "https://example.com" {
		t.Errorf("refs = %v, want [https://example.com]", pf.Refs)
	}
}

func TestSQLParseFact_DefaultType(t *testing.T) {
	db := openTestDB(t)

	blob := "---\ndomain: [go]\n---\n# Default Type Test\n\nBody."

	var result sql.NullString
	err := db.QueryRow("SELECT knomit_parse_fact(?)", []byte(blob)).Scan(&result)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !result.Valid {
		t.Fatal("expected non-NULL result")
	}

	var pf parsedFact
	if err := json.Unmarshal([]byte(result.String), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pf.Type != "observation" {
		t.Errorf("type = %q, want %q", pf.Type, "observation")
	}
}

func TestSQLParseFact_NotAFact(t *testing.T) {
	db := openTestDB(t)

	blob := "# Just a heading\n\nNo frontmatter here."

	var result sql.NullString
	err := db.QueryRow("SELECT knomit_parse_fact(?)", []byte(blob)).Scan(&result)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Valid {
		t.Errorf("expected NULL, got %q", result.String)
	}
}

func TestSQLCosineSim_IdenticalVectors(t *testing.T) {
	a := float32SliceToBytes([]float32{1, 2, 3, 4})
	result := sqlCosineSim(a, a)
	if result == nil {
		t.Fatal("expected non-nil result for identical vectors")
	}
	got := result.(float64)
	if math.Abs(got-1.0) > 1e-6 {
		t.Errorf("identical vectors cosine = %f, want 1.0", got)
	}
}

func TestSQLCosineSim_OrthogonalVectors(t *testing.T) {
	a := float32SliceToBytes([]float32{1, 0, 0, 0})
	b := float32SliceToBytes([]float32{0, 1, 0, 0})
	result := sqlCosineSim(a, b)
	if result == nil {
		t.Fatal("expected non-nil result for orthogonal vectors")
	}
	got := result.(float64)
	if math.Abs(got) > 1e-6 {
		t.Errorf("orthogonal vectors cosine = %f, want 0.0", got)
	}
}

func TestSQLCosineSim_KnownValue(t *testing.T) {
	// [1,0] · [1,1]/sqrt(2) = 1/sqrt(2) ≈ 0.7071
	a := float32SliceToBytes([]float32{1, 0})
	b := float32SliceToBytes([]float32{1, 1})
	result := sqlCosineSim(a, b)
	if result == nil {
		t.Fatal("expected non-nil")
	}
	want := 1.0 / math.Sqrt2
	got := result.(float64)
	if math.Abs(got-want) > 1e-5 {
		t.Errorf("cosine = %f, want %f", got, want)
	}
}

func TestSQLCosineSim_NilInput(t *testing.T) {
	a := float32SliceToBytes([]float32{1, 2, 3})
	if sqlCosineSim(nil, a) != nil {
		t.Error("expected nil for nil first arg")
	}
	if sqlCosineSim(a, nil) != nil {
		t.Error("expected nil for nil second arg")
	}
	if sqlCosineSim(nil, nil) != nil {
		t.Error("expected nil for both nil")
	}
}

func TestSQLCosineSim_MismatchedLengths(t *testing.T) {
	a := float32SliceToBytes([]float32{1, 2})
	b := float32SliceToBytes([]float32{1, 2, 3})
	if sqlCosineSim(a, b) != nil {
		t.Error("expected nil for mismatched lengths")
	}
}

func TestSQLCosineSim_ZeroVector(t *testing.T) {
	a := float32SliceToBytes([]float32{0, 0, 0})
	b := float32SliceToBytes([]float32{1, 2, 3})
	if sqlCosineSim(a, b) != nil {
		t.Error("expected nil when one vector is zero (denom=0)")
	}
}

func TestSQLParseFact_NoTitle(t *testing.T) {
	db := openTestDB(t)

	blob := "---\ndomain: [go]\nconfidence: 0.8\n---\n\nNo heading here, just body text."

	var result sql.NullString
	err := db.QueryRow("SELECT knomit_parse_fact(?)", []byte(blob)).Scan(&result)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Valid {
		t.Errorf("expected NULL, got %q", result.String)
	}
}
