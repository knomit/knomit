package synthesize

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/store"
)

func TestRunPruneOnly(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	files := map[string]string{
		"know/test/keep.md":   factContent("Keep fact", "This should be kept."),
		"know/test/forget.md": factContent("Forget fact", "This is obsolete."),
	}

	gs.EXPECT().ListAll().Return([]string{"know/test/keep.md", "know/test/forget.md"}, nil)
	gs.EXPECT().ReadFile(gomock.Any()).DoAndReturn(func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("not found: %s", path)
	}).AnyTimes()

	// Louvain clustering returns both facts in one cluster.
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"know/test/keep.md", "know/test/forget.md"}},
	}, nil)

	llmResp := `{
  "decisions": [
    { "path": "know/test/keep.md", "action": "keep" },
    { "path": "know/test/forget.md", "action": "forget" }
  ],
  "merges": []
}`
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(llmResp, nil)

	// forget.md: DeleteFile + idx.Delete
	gs.EXPECT().DeleteFile("know/test/forget.md", gomock.Any()).Return(nil)
	idx.EXPECT().Delete("know/test/forget.md").Return(nil)

	// Tag
	gs.EXPECT().Tag("learn/synthesize-smoke-prune-prune").Return(nil)

	recipe := Recipe{
		Name:  "smoke-prune",
		Steps: []RecipeStep{{Mode: "prune"}},
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
}

func TestRunDistillNoEmbeddings(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	// Empty index → no facts → distill returns early without calling LLM.
	idx.EXPECT().Search(gomock.Any()).Return([]store.SearchResult{}, nil)

	recipe := Recipe{
		Name:  "smoke-distill",
		Steps: []RecipeStep{{Mode: "distill"}},
	}

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
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	recipe := Recipe{
		Name:  "bad",
		Steps: []RecipeStep{{Mode: "unknown"}},
	}
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, nil)
	if err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

func TestRunDistillWithFacts(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	searchResults := []store.SearchResult{
		{FactRecord: store.FactRecord{Path: "know/test/a.md", Title: "A fact", Body: "A body.", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}},
		{FactRecord: store.FactRecord{Path: "know/test/b.md", Title: "B fact", Body: "B body.", Domain: []string{"testing"}, Confidence: 0.8, Sources: 1}},
	}
	// ClusterFacts returns both facts in one cluster.
	idx.EXPECT().Search(gomock.Any()).Return(searchResults, nil)
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"know/test/a.md", "know/test/b.md"}},
	}, nil)

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
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(llmResp, nil)

	// Write synthesized fact
	var synthWritten bool
	gs.EXPECT().WriteFile("know/test/synth.md", gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg string) error {
		synthWritten = true
		return nil
	})
	gs.EXPECT().HeadCommit().Return("deadbeef", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	idx.EXPECT().GraphAddDerivedFrom("know/test/synth.md", gomock.Any()).Return(nil)

	// Delete forgotten fact
	gs.EXPECT().DeleteFile("know/test/a.md", gomock.Any()).Return(nil)
	idx.EXPECT().Delete("know/test/a.md").Return(nil)

	// Tag
	gs.EXPECT().Tag("learn/synthesize-distill-with-facts-distill").Return(nil)

	recipe := Recipe{
		Name:  "distill-with-facts",
		Steps: []RecipeStep{{Mode: "distill"}},
	}

	var phases []string
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, func(e ProgressEvent) {
		phases = append(phases, e.Phase)
	})
	if err != nil {
		t.Fatalf("Run distill: %v", err)
	}

	if !synthWritten {
		t.Errorf("expected know/test/synth.md to be written")
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
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	// Empty store, no facts — prune returns early after gather (no Tag call).
	gs.EXPECT().ListAll().Return([]string{}, nil)

	recipe := Recipe{
		Name:  "nil-progress",
		Steps: []RecipeStep{{Mode: "prune"}},
	}
	// Empty store, no facts — should complete without panic.
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, nil)
	if err != nil {
		t.Fatalf("Run with nil progress: %v", err)
	}
}
