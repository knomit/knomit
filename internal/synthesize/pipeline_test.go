package synthesize

import (
	"context"
	"testing"

	"knomit/internal/store"
)

func TestRunPruneOnly(t *testing.T) {
	gs := newMockGitStore()
	gs.files["know/test/keep.md"] = factContent("Keep fact", "This should be kept.")
	gs.files["know/test/forget.md"] = factContent("Forget fact", "This is obsolete.")

	llmResp := `{
  "decisions": [
    { "path": "know/test/keep.md", "action": "keep" },
    { "path": "know/test/forget.md", "action": "forget" }
  ],
  "merges": []
}`
	adapter := &mockLLM{response: llmResp}
	idx := &mockSearchIndex{}

	recipe := Recipe{
		Name: "smoke-prune",
		Steps: []RecipeStep{
			{Mode: "prune"},
		},
	}

	var phases []string
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, func(e ProgressEvent) {
		phases = append(phases, e.Phase)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify done event was emitted.
	doneSeen := false
	for _, p := range phases {
		if p == "done" {
			doneSeen = true
		}
	}
	if !doneSeen {
		t.Errorf("expected 'done' phase; phases: %v", phases)
	}

	// forget.md should be deleted.
	forgetDeleted := false
	for _, d := range gs.deleted {
		if d == "know/test/forget.md" {
			forgetDeleted = true
		}
	}
	if !forgetDeleted {
		t.Errorf("expected know/test/forget.md to be deleted; deleted: %v", gs.deleted)
	}
}

func TestRunDistillNoEmbeddings(t *testing.T) {
	gs := newMockGitStore()
	idx := &mockSearchIndex{} // empty index → no facts → early return

	recipe := Recipe{
		Name: "smoke-distill",
		Steps: []RecipeStep{
			{Mode: "distill"},
		},
	}

	// LLM should not be called since there are no facts.
	adapter := &mockLLM{response: "should not be called"}

	var phases []string
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, func(e ProgressEvent) {
		phases = append(phases, e.Phase)
	})
	if err != nil {
		t.Fatalf("Run distill: %v", err)
	}

	doneSeen := false
	for _, p := range phases {
		if p == "done" {
			doneSeen = true
		}
	}
	if !doneSeen {
		t.Errorf("expected 'done' phase; phases: %v", phases)
	}
}

func TestRunUnknownMode(t *testing.T) {
	gs := newMockGitStore()
	idx := &mockSearchIndex{}
	recipe := Recipe{
		Name:  "bad",
		Steps: []RecipeStep{{Mode: "unknown"}},
	}
	err := Run(context.Background(), gs, idx, nil, &mockLLM{}, recipe, nil)
	if err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

// mockIndexWithEmbeddings wraps mockSearchIndex and adds GetEmbedding support.
type mockIndexWithEmbeddings struct {
	mockSearchIndex
	results    []store.SearchResult
	embeddings map[string][]float32
}

func (m *mockIndexWithEmbeddings) Search(_ store.SearchQuery) ([]store.SearchResult, error) {
	return m.results, nil
}

func (m *mockIndexWithEmbeddings) GetEmbedding(path string) ([]float32, error) {
	return m.embeddings[path], nil
}

func TestRunDistillWithFacts(t *testing.T) {
	gs := newMockGitStore()

	llmResp := `{
  "synthesize": [
    {
      "path": "know/test/synth.md",
      "title": "Synthesized insight",
      "body": "Combined understanding.",
      "domain": ["testing"],
      "confidence": 0.9,
      "entities": [],
      "refs": ["know/test/a.md", "know/test/b.md"]
    }
  ],
  "forget": ["know/test/a.md"]
}`
	adapter := &mockLLM{response: llmResp}

	idx := &mockIndexWithEmbeddings{
		results: []store.SearchResult{
			{FactRecord: store.FactRecord{Path: "know/test/a.md", Title: "A fact", Body: "A body.", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}},
			{FactRecord: store.FactRecord{Path: "know/test/b.md", Title: "B fact", Body: "B body.", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}},
		},
		embeddings: map[string][]float32{}, // no embeddings → single cluster path
	}

	recipe := Recipe{
		Name:  "distill-with-facts",
		Steps: []RecipeStep{{Mode: "distill", MinClusterSize: 3}},
	}

	var phases []string
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, func(e ProgressEvent) {
		phases = append(phases, e.Phase)
	})
	if err != nil {
		t.Fatalf("Run distill: %v", err)
	}

	// Synthesized fact should be written to git.
	if _, ok := gs.written["know/test/synth.md"]; !ok {
		t.Errorf("expected know/test/synth.md to be written; written: %v", gs.written)
	}

	// Forgotten fact should be deleted.
	aDeleted := false
	for _, d := range gs.deleted {
		if d == "know/test/a.md" {
			aDeleted = true
		}
	}
	if !aDeleted {
		t.Errorf("expected know/test/a.md to be deleted; deleted: %v", gs.deleted)
	}

	// Tag should be applied.
	tagFound := false
	for _, tag := range gs.tags {
		if tag == "learn/synthesize-distill-with-facts-distill" {
			tagFound = true
		}
	}
	if !tagFound {
		t.Errorf("expected distill tag; tags: %v", gs.tags)
	}

	// done event emitted.
	doneSeen := false
	for _, p := range phases {
		if p == "done" {
			doneSeen = true
		}
	}
	if !doneSeen {
		t.Errorf("expected 'done' phase; phases: %v", phases)
	}
}

func TestRunNilProgress(t *testing.T) {
	gs := newMockGitStore()
	idx := &mockSearchIndex{}
	recipe := Recipe{
		Name:  "nil-progress",
		Steps: []RecipeStep{{Mode: "prune"}},
	}
	// Empty store, no facts — should complete without panic.
	err := Run(context.Background(), gs, idx, nil, &mockLLM{response: `{"decisions":[],"merges":[]}`}, recipe, nil)
	if err != nil {
		t.Fatalf("Run with nil progress: %v", err)
	}
}
