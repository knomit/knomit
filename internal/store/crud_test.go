package store

import (
	"fmt"
	"testing"
)

// ── RecentFacts ──────────────────────────────────────────────────────────────

// insertRecentFact is a test helper that inserts a blob + fact record for
// RecentFacts tests. commit_log is not populated so committed_at defaults to 0.
func insertRecentFact(t *testing.T, idx *Index, path, typ string, confidence float64) {
	t.Helper()
	bh := "bh_" + path
	insertBlob(t, idx.db, bh, "content of "+path)
	if err := idx.Upsert(FactRecord{
		Path:       path,
		Title:      path,
		BlobHash:   bh,
		Type:       typ,
		Domain:     []string{"test"},
		Entities:   []string{},
		Confidence: confidence,
		Sources:    1,
		CommitHash: "abc",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecentFacts_BasicListing(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFact(t, idx, "kb/alpha/one.md", "observation", 0.9)
	insertRecentFact(t, idx, "kb/alpha/two.md", "hypothesis", 0.8)
	insertRecentFact(t, idx, "kb/beta/three.md", "observation", 0.7)

	entries, total, err := idx.RecentFacts("kb/alpha", "", 10, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2", total)
	}
	if len(entries) != 2 {
		t.Errorf("len(entries)=%d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Path == "kb/beta/three.md" {
			t.Errorf("kb/beta/three.md should not appear under kb/alpha prefix")
		}
	}
}

func TestRecentFacts_IncludeTypes(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFact(t, idx, "kb/obs.md", "observation", 0.9)
	insertRecentFact(t, idx, "kb/hyp.md", "hypothesis", 0.8)
	insertRecentFact(t, idx, "kb/syn.md", "synthesis", 0.7)

	entries, total, err := idx.RecentFacts("kb", "", 10, 0, []string{"observation"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("total=%d, want 1", total)
	}
	if len(entries) != 1 || entries[0].Type != "observation" {
		t.Errorf("expected one observation entry, got %v", entries)
	}
}

func TestRecentFacts_ExcludeTypes(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFact(t, idx, "kb/obs.md", "observation", 0.9)
	insertRecentFact(t, idx, "kb/hyp.md", "hypothesis", 0.8)
	insertRecentFact(t, idx, "kb/syn.md", "synthesis", 0.7)

	entries, total, err := idx.RecentFacts("kb", "", 10, 0, nil, []string{"hypothesis"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2", total)
	}
	for _, e := range entries {
		if e.Type == "hypothesis" {
			t.Errorf("hypothesis should be excluded, got entry %v", e)
		}
	}
}

func TestRecentFacts_Pagination(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	for i := 0; i < 5; i++ {
		insertRecentFact(t, idx, fmt.Sprintf("kb/f%02d.md", i), "observation", 0.9)
	}

	// Page 1.
	entries, total, err := idx.RecentFacts("kb", "", 2, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	if len(entries) != 2 {
		t.Errorf("page 1 len=%d, want 2", len(entries))
	}

	// Page 2.
	entries, total, err = idx.RecentFacts("kb", "", 2, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	if len(entries) != 2 {
		t.Errorf("page 2 len=%d, want 2", len(entries))
	}

	// Last page (1 item).
	entries, total, err = idx.RecentFacts("kb", "", 2, 4, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	if len(entries) != 1 {
		t.Errorf("last page len=%d, want 1", len(entries))
	}
}

func TestRecentFacts_EmptyPrefix(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFact(t, idx, "kb/one.md", "observation", 0.9)
	insertRecentFact(t, idx, "notes/two.md", "hypothesis", 0.8)

	// Empty prefix should return all facts.
	_, total, err := idx.RecentFacts("", "", 100, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2 with empty prefix", total)
	}
}

func TestRecentFacts_SearchPath(t *testing.T) {
	// Exercises recentFactsSearch (query != "").
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Stub embedder: "match" gets a vector close to the query; "other" gets a
	// distant vector so it falls below the similarity threshold.
	emb := &stubEmbedder4d{}
	idx.SetEmbedder(emb)

	insertBlob(t, idx.db, "bh_match", "alpha")
	insertBlob(t, idx.db, "bh_other", "gamma") // far from query "alpha"
	if err := idx.Upsert(FactRecord{
		Path: "kb/match.md", Title: "alpha", BlobHash: "bh_match",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1, CommitHash: "c1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(FactRecord{
		Path: "kb/other.md", Title: "gamma", BlobHash: "bh_other",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1, CommitHash: "c2",
	}); err != nil {
		t.Fatal(err)
	}

	entries, total, err := idx.RecentFacts("kb", "alpha", 10, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("expected at least one result from search path")
	}
	if len(entries) > total {
		t.Errorf("len(entries)=%d > total=%d", len(entries), total)
	}
	// The match entry should be present.
	found := false
	for _, e := range entries {
		if e.Path == "kb/match.md" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("kb/match.md not found in search results: %v", entries)
	}
}

func TestRecentFacts_SearchPath_Pagination(t *testing.T) {
	// recentFactsSearch paginates in Go after fetching all results.
	idx, err := New(":memory:", WithVecDimension(4))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// stubEmbedder4d is defined in graph_test.go (same package).
	idx.SetEmbedder(&stubEmbedder4d{})

	// Insert 5 facts that all embed to the same vector (all will match).
	for i := 0; i < 5; i++ {
		bh := fmt.Sprintf("bh_s%d", i)
		path := fmt.Sprintf("kb/s%02d.md", i)
		insertBlob(t, idx.db, bh, fmt.Sprintf("fact %d content", i))
		if err := idx.Upsert(FactRecord{
			Path: path, Title: fmt.Sprintf("fact %d", i), BlobHash: bh,
			Type: "observation", Domain: []string{"test"}, Entities: []string{},
			Confidence: 0.9, Sources: 1, CommitHash: "abc",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Request page 1 (limit=2, offset=0).
	entries, total, err := idx.RecentFacts("kb", "query", 2, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	if len(entries) != 2 {
		t.Errorf("page 1 len=%d, want 2", len(entries))
	}

	// Offset beyond total returns empty slice with correct total.
	entries, total, err = idx.RecentFacts("kb", "query", 2, 10, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Errorf("total=%d, want 5", total)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries for offset beyond total, got %d", len(entries))
	}
}

// ── GraphAddDerivedFrom ───────────────────────────────────────────────────────

func TestGraphAddDerivedFrom(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Register facts as graph nodes via graphSyncFact.
	newFact := FactRecord{
		Path: "kb/new.md", Title: "New", BlobHash: "bh_new",
		Type: "synthesis", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}
	src1 := FactRecord{
		Path: "kb/src1.md", Title: "Source 1", BlobHash: "bh_src1",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}
	src2 := FactRecord{
		Path: "kb/src2.md", Title: "Source 2", BlobHash: "bh_src2",
		Type: "observation", Domain: []string{"test"}, Entities: []string{},
		Confidence: 0.9, Sources: 1,
	}

	tx, err := idx.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []FactRecord{newFact, src1, src2} {
		if err := idx.graphSyncFactTx(tx, rec); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Create DERIVED_FROM edges.
	if err := idx.GraphAddDerivedFrom("kb/new.md", []string{"kb/src1.md", "kb/src2.md"}); err != nil {
		t.Fatalf("GraphAddDerivedFrom: %v", err)
	}

	// Verify two DERIVED_FROM edges from kb/new.md.
	var count int
	err = idx.db.QueryRow(
		`SELECT count(*) FROM json_each(cypher('MATCH (:Fact {path: "kb/new.md"})-[:DERIVED_FROM]->(:Fact) RETURN 1 AS n'))`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("cypher query: %v", err)
	}
	if count != 2 {
		t.Errorf("DERIVED_FROM edge count=%d, want 2", count)
	}
}

func TestGraphAddDerivedFrom_EmptySources(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// No-op: empty sourcePaths should not error.
	if err := idx.GraphAddDerivedFrom("kb/new.md", nil); err != nil {
		t.Errorf("unexpected error for empty sources: %v", err)
	}
	if err := idx.GraphAddDerivedFrom("kb/new.md", []string{}); err != nil {
		t.Errorf("unexpected error for empty sources slice: %v", err)
	}
}
