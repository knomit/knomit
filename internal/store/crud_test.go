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

	entries, total, err := idx.RecentFacts("kb/alpha", "", 10, 0, nil, nil, nil, nil, nil)
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

	entries, total, err := idx.RecentFacts("kb", "", 10, 0, []string{"observation"}, nil, nil, nil, nil)
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

	entries, total, err := idx.RecentFacts("kb", "", 10, 0, nil, []string{"hypothesis"}, nil, nil, nil)
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
	entries, total, err := idx.RecentFacts("kb", "", 2, 0, nil, nil, nil, nil, nil)
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
	entries, total, err = idx.RecentFacts("kb", "", 2, 2, nil, nil, nil, nil, nil)
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
	entries, total, err = idx.RecentFacts("kb", "", 2, 4, nil, nil, nil, nil, nil)
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
	_, total, err := idx.RecentFacts("", "", 100, 0, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2 with empty prefix", total)
	}
}

func TestRecentFacts_SearchPath(t *testing.T) {
	// Exercises recentFactsSearch (query != "").
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Stub embedder: "match" gets a vector close to the query; "other" gets a
	// distant vector so it falls below the similarity threshold.
	emb := &stubEmbedder768d{}
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

	entries, total, err := idx.RecentFacts("kb", "alpha", 10, 0, nil, nil, nil, nil, nil)
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
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// stubEmbedder768d is defined in graph_test.go (same package).
	idx.SetEmbedder(&stubEmbedder768d{})

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
	entries, total, err := idx.RecentFacts("kb", "query", 2, 0, nil, nil, nil, nil, nil)
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
	entries, total, err = idx.RecentFacts("kb", "query", 2, 10, nil, nil, nil, nil, nil)
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

// insertRecentFactFull is like insertRecentFact but allows custom domain, entities,
// and inserts a commit_log row so the operation field is populated.
func insertRecentFactFull(t *testing.T, idx *Index, path, typ string, domain, entities []string, commitHash, operation string) {
	t.Helper()
	bh := "bh_" + path
	insertBlob(t, idx.db, bh, "content of "+path)
	if err := idx.Upsert(FactRecord{
		Path:       path,
		Title:      path,
		BlobHash:   bh,
		Type:       typ,
		Domain:     domain,
		Entities:   entities,
		Confidence: 0.9,
		Sources:    1,
		CommitHash: commitHash,
	}); err != nil {
		t.Fatal(err)
	}
	if operation != "" {
		_, err := idx.db.Exec(
			`INSERT OR IGNORE INTO commit_log(commit_hash, path, committed_at, message, operation, action) VALUES (?, ?, 1000, ?, ?, ?)`,
			commitHash, path, "test commit", operation, operation,
		)
		if err != nil {
			t.Fatalf("insertRecentFactFull commit_log %s: %v", path, err)
		}
	}
}

func TestRecentFacts_DomainFilter(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFactFull(t, idx, "kb/go.md", "observation", []string{"go"}, []string{}, "c1", "learn")
	insertRecentFactFull(t, idx, "kb/rust.md", "observation", []string{"rust"}, []string{}, "c2", "learn")
	insertRecentFactFull(t, idx, "kb/go-perf.md", "observation", []string{"go/performance"}, []string{}, "c3", "learn")

	// Filter by top-level domain "go" — should match "go" and "go/performance" (LIKE pattern).
	entries, total, err := idx.RecentFacts("kb", "", 10, 0, nil, nil, []string{"go"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2 (go + go/performance)", total)
	}
	if len(entries) != 2 {
		t.Errorf("len(entries)=%d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Path == "kb/rust.md" {
			t.Errorf("rust fact should be excluded by domain filter")
		}
	}
}

func TestRecentFacts_EntityFilter(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFactFull(t, idx, "kb/chi.md", "observation", []string{"go"}, []string{"chi", "router"}, "c1", "learn")
	insertRecentFactFull(t, idx, "kb/gin.md", "observation", []string{"go"}, []string{"gin", "router"}, "c2", "learn")
	insertRecentFactFull(t, idx, "kb/other.md", "observation", []string{"go"}, []string{"other"}, "c3", "learn")

	// Filter by entity "router" — should match chi and gin.
	entries, total, err := idx.RecentFacts("kb", "", 10, 0, nil, nil, nil, []string{"router"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2 (chi + gin)", total)
	}
	if len(entries) != 2 {
		t.Errorf("len(entries)=%d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Path == "kb/other.md" {
			t.Errorf("other fact should be excluded by entity filter")
		}
	}
}

func TestRecentFacts_EpFilter(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	insertRecentFactFull(t, idx, "kb/learned.md", "observation", []string{"test"}, []string{}, "c1", "learn")
	insertRecentFactFull(t, idx, "kb/updated.md", "observation", []string{"test"}, []string{}, "c2", "update")
	insertRecentFactFull(t, idx, "kb/retracted.md", "observation", []string{"test"}, []string{}, "c3", "retract")

	// Filter ep=learn — only the learned fact.
	entries, total, err := idx.RecentFacts("kb", "", 10, 0, nil, nil, nil, nil, []string{"learn"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("total=%d, want 1 (learn only)", total)
	}
	if len(entries) != 1 || entries[0].Path != "kb/learned.md" {
		t.Errorf("expected kb/learned.md, got %v", entries)
	}

	// Filter ep=learn,update — two facts.
	entries, total, err = idx.RecentFacts("kb", "", 10, 0, nil, nil, nil, nil, []string{"learn", "update"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total=%d, want 2 (learn + update)", total)
	}
	_ = entries
}

