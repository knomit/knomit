package synthesize

import (
	"context"
	"testing"

	gomock "go.uber.org/mock/gomock"
	"knomit/internal/store"
)

// TestMergeFacts_HigherConfidenceWins verifies that when A has higher confidence,
// A is the winner and fields are merged correctly.
func TestMergeFacts_HigherConfidenceWins(t *testing.T) {
	a := factForLLM{
		File:       "kb/a.md",
		Title:      "Fact A",
		Body:       "Body of A",
		Domain:     []string{"domain-a"},
		Entities:   []string{"entity-a"},
		Confidence: 0.9,
		Sources:    2,
	}
	b := factForLLM{
		File:       "kb/b.md",
		Title:      "Fact B",
		Body:       "Body of B",
		Domain:     []string{"domain-b"},
		Entities:   []string{"entity-b"},
		Confidence: 0.8,
		Sources:    3,
	}

	winner, loser := mergeFacts(a, b)

	if winner.File != "kb/a.md" {
		t.Errorf("expected winner file=kb/a.md, got %q", winner.File)
	}
	if winner.Title != "Fact A" {
		t.Errorf("expected winner title=Fact A, got %q", winner.Title)
	}
	if winner.Body != "Body of A" {
		t.Errorf("expected winner body=Body of A, got %q", winner.Body)
	}
	if loser.File != "kb/b.md" {
		t.Errorf("expected loser file=kb/b.md, got %q", loser.File)
	}

	// Sources should be summed.
	if winner.Sources != 5 {
		t.Errorf("expected winner sources=5, got %d", winner.Sources)
	}
	// Confidence should be max.
	if winner.Confidence != 0.9 {
		t.Errorf("expected winner confidence=0.9, got %f", winner.Confidence)
	}
	// Domains should be union.
	domainSet := make(map[string]bool)
	for _, d := range winner.Domain {
		domainSet[d] = true
	}
	if !domainSet["domain-a"] || !domainSet["domain-b"] {
		t.Errorf("expected winner domains to include both domain-a and domain-b, got %v", winner.Domain)
	}
	// Entities should be union.
	entitySet := make(map[string]bool)
	for _, e := range winner.Entities {
		entitySet[e] = true
	}
	if !entitySet["entity-a"] || !entitySet["entity-b"] {
		t.Errorf("expected winner entities to include both entity-a and entity-b, got %v", winner.Entities)
	}
}

// TestMergeFacts_TieBreakBySources verifies that when confidence is equal,
// the fact with more sources wins.
func TestMergeFacts_TieBreakBySources(t *testing.T) {
	a := factForLLM{
		File:       "kb/a.md",
		Title:      "Fact A",
		Body:       "Body of A",
		Domain:     []string{"domain-a"},
		Entities:   []string{},
		Confidence: 0.8,
		Sources:    1,
	}
	b := factForLLM{
		File:       "kb/b.md",
		Title:      "Fact B",
		Body:       "Body of B",
		Domain:     []string{"domain-b"},
		Entities:   []string{},
		Confidence: 0.8,
		Sources:    3,
	}

	winner, loser := mergeFacts(a, b)

	// B has more sources, so B wins.
	if winner.File != "kb/b.md" {
		t.Errorf("expected winner file=kb/b.md (higher sources), got %q", winner.File)
	}
	if loser.File != "kb/a.md" {
		t.Errorf("expected loser file=kb/a.md, got %q", loser.File)
	}
	if winner.Sources != 4 {
		t.Errorf("expected merged sources=4, got %d", winner.Sources)
	}
}

// TestMergeFacts_DeduplicatesDomains verifies that overlapping domains are
// deduplicated in the merged winner.
func TestMergeFacts_DeduplicatesDomains(t *testing.T) {
	a := factForLLM{
		File:       "kb/a.md",
		Title:      "Fact A",
		Body:       "Body of A",
		Domain:     []string{"shared", "only-a"},
		Entities:   []string{},
		Confidence: 0.9,
		Sources:    1,
	}
	b := factForLLM{
		File:       "kb/b.md",
		Title:      "Fact B",
		Body:       "Body of B",
		Domain:     []string{"shared", "only-b"},
		Entities:   []string{},
		Confidence: 0.8,
		Sources:    1,
	}

	winner, _ := mergeFacts(a, b)

	// Count occurrences of "shared" to ensure no duplicates.
	sharedCount := 0
	for _, d := range winner.Domain {
		if d == "shared" {
			sharedCount++
		}
	}
	if sharedCount != 1 {
		t.Errorf("expected 'shared' domain to appear exactly once, got %d times in %v", sharedCount, winner.Domain)
	}
	// All three distinct domains should be present.
	domainSet := make(map[string]bool)
	for _, d := range winner.Domain {
		domainSet[d] = true
	}
	for _, expected := range []string{"shared", "only-a", "only-b"} {
		if !domainSet[expected] {
			t.Errorf("expected domain %q to be present in merged domains %v", expected, winner.Domain)
		}
	}
}

// TestBuildMergePairs_GreedyConsumption verifies that when A≈B and B≈C,
// only the higher-similarity pair (A,B) is selected and C is not consumed.
func TestBuildMergePairs_GreedyConsumption(t *testing.T) {
	a := factForLLM{File: "kb/a.md", Title: "A", Body: "body a"}
	b := factForLLM{File: "kb/b.md", Title: "B", Body: "body b"}
	c := factForLLM{File: "kb/c.md", Title: "C", Body: "body c"}

	pairs := []mergePair{
		{a: a, b: b, similarity: 0.95},
		{a: b, b: c, similarity: 0.93},
	}

	selected := applyGreedyMerges(pairs)

	if len(selected) != 1 {
		t.Fatalf("expected 1 selected pair, got %d", len(selected))
	}
	// The highest-similarity pair (A,B) should be selected.
	got := selected[0]
	if !(got.a.File == "kb/a.md" && got.b.File == "kb/b.md") &&
		!(got.a.File == "kb/b.md" && got.b.File == "kb/a.md") {
		t.Errorf("expected selected pair to be (a,b), got (%s, %s)", got.a.File, got.b.File)
	}
}

// TestDedupCluster_MergesNearDuplicates is a full integration test:
// 3 facts where A and B are near-duplicates (above threshold) and C is unrelated.
// After dedup, 2 facts should survive.
func TestDedupCluster_MergesNearDuplicates(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	factA := factForLLM{
		File:       "kb/a.md",
		Title:      "Fact A",
		Body:       "Body of fact A about topic X.",
		Domain:     []string{"testing"},
		Entities:   []string{"entity-a"},
		Confidence: 0.9,
		Sources:    2,
	}
	factB := factForLLM{
		File:       "kb/b.md",
		Title:      "Fact B",
		Body:       "Body of fact B about topic X, similar to A.",
		Domain:     []string{"testing"},
		Entities:   []string{"entity-b"},
		Confidence: 0.8,
		Sources:    1,
	}
	factC := factForLLM{
		File:       "kb/c.md",
		Title:      "Unrelated Fact C",
		Body:       "Something completely different.",
		Domain:     []string{"other"},
		Entities:   []string{},
		Confidence: 0.75,
		Sources:    1,
	}

	cluster := []factForLLM{factA, factB, factC}

	// factA content (winner — higher confidence)
	aContent := "---\ndomain: [testing]\nconfidence: 0.9\nsources: 2\nentities: [entity-a]\nrefs: []\n---\n# Fact A\n\nBody of fact A about topic X.\n"
	// factB content (loser)
	bContent := "---\ndomain: [testing]\nconfidence: 0.8\nsources: 1\nentities: [entity-b]\nrefs: []\n---\n# Fact B\n\nBody of fact B about topic X, similar to A.\n"

	// Search for A → returns B (score 95)
	idx.EXPECT().Search(store.SearchQuery{
		Text:          factA.Title + " " + factA.Body,
		MinSimilarity: defaultDedupThreshold,
		Limit:         10,
	}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/b.md", Title: "Fact B"}}, Score: 95},
	}, nil)

	// Search for B → returns A (score 95)
	idx.EXPECT().Search(store.SearchQuery{
		Text:          factB.Title + " " + factB.Body,
		MinSimilarity: defaultDedupThreshold,
		Limit:         10,
	}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/a.md", Title: "Fact A"}}, Score: 95},
	}, nil)

	// Search for C → no matches
	idx.EXPECT().Search(store.SearchQuery{
		Text:          factC.Title + " " + factC.Body,
		MinSimilarity: defaultDedupThreshold,
		Limit:         10,
	}).Return([]store.SearchResult{}, nil)

	// Winner is A (higher confidence). Read both to get full facts with Refs.
	gs.EXPECT().ReadFile("kb/a.md").Return(aContent, nil)
	gs.EXPECT().ReadFile("kb/b.md").Return(bContent, nil)

	// Write merged winner back to git.
	gs.EXPECT().WriteFile("kb/a.md", gomock.Any(), gomock.Any(), gomock.Any()).Return("commit-hash-1", "blob-hash-1", nil)

	// Upsert updated winner into index.
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	// Delete loser from git and index.
	gs.EXPECT().DeleteFile("kb/b.md", gomock.Any(), gomock.Any()).Return("commit-hash-2", nil)
	idx.EXPECT().Delete("kb/b.md").Return(nil)

	surviving, err := dedupCluster(context.Background(), cluster, gs, idx, defaultDedupThreshold, "test-recipe", func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("dedupCluster: %v", err)
	}

	if len(surviving) != 2 {
		t.Errorf("expected 2 surviving facts, got %d: %v", len(surviving), func() []string {
			var paths []string
			for _, f := range surviving {
				paths = append(paths, f.File)
			}
			return paths
		}())
	}

	// B should not be in the survivors.
	for _, f := range surviving {
		if f.File == "kb/b.md" {
			t.Errorf("loser kb/b.md should not be in surviving facts")
		}
	}
}

// TestDedupCluster_SkipsBelowThreshold verifies that when no facts exceed the
// similarity threshold, all facts survive unchanged.
func TestDedupCluster_SkipsBelowThreshold(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	factA := factForLLM{File: "kb/a.md", Title: "Fact A", Body: "Body A"}
	factB := factForLLM{File: "kb/b.md", Title: "Fact B", Body: "Body B"}

	cluster := []factForLLM{factA, factB}

	// Both searches return empty (no near-duplicates).
	idx.EXPECT().Search(gomock.Any()).Return([]store.SearchResult{}, nil).Times(2)

	// No git or index mutations should happen.
	_ = gs // no expectations set — gomock will fail if any method is called

	surviving, err := dedupCluster(context.Background(), cluster, gs, idx, defaultDedupThreshold, "test-recipe", func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("dedupCluster: %v", err)
	}

	if len(surviving) != 2 {
		t.Errorf("expected 2 surviving facts (no merges), got %d", len(surviving))
	}
}
