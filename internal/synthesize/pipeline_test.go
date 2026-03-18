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
	adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()

	files := map[string]string{
		"kb/test/keep.md":   factContent("Keep fact", "This should be kept."),
		"kb/test/forget.md": factContent("Forget fact", "This is obsolete."),
	}

	gs.EXPECT().ListAll().Return([]string{"kb/test/keep.md", "kb/test/forget.md"}, nil)
	gs.EXPECT().ReadFile(gomock.Any()).DoAndReturn(func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("not found: %s", path)
	}).AnyTimes()

	// Louvain clustering returns both facts in one cluster.
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"kb/test/keep.md", "kb/test/forget.md"}},
	}, nil)
	// Dedup pass: no near-duplicates found.
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil).AnyTimes()

	llmResp := `{
  "decisions": [
    { "path": "kb/test/keep.md", "action": "keep" },
    { "path": "kb/test/forget.md", "action": "retract" }
  ],
  "merges": []
}`
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(llmResp, nil)

	// forget.md: DeleteFile + idx.Delete
	gs.EXPECT().DeleteFile("kb/test/forget.md", gomock.Any(), gomock.Any()).Return("deadbeef", nil)
	idx.EXPECT().Delete("kb/test/forget.md").Return(nil)

	// Tag per operation (retract for the deleted fact)


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
	adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()

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
	adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()

	recipe := Recipe{
		Name:  "bad",
		Steps: []RecipeStep{{Mode: "unknown"}},
	}
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, nil)
	if err == nil {
		t.Error("expected error for unknown mode, got nil")
	}
}

func testSearchResult(path, title, body string) store.SearchResult {
	return store.SearchResult{
		FactWithBody: store.FactWithBody{
			FactRecord: store.FactRecord{
				Path: path, Title: title,
				Domain: []string{"testing"}, Confidence: 0.8, Sources: 1,
			},
			Body: body,
		},
	}
}

func TestRunDistillWithFacts(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)
	adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()

	searchResults := []store.SearchResult{
		testSearchResult("kb/test/a.md", "A fact", "A body."),
		testSearchResult("kb/test/b.md", "B fact", "B body."),
	}
	// ClusterFacts returns both facts in one cluster.
	idx.EXPECT().Search(gomock.Any()).Return(searchResults, nil)
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"kb/test/a.md", "kb/test/b.md"}},
	}, nil)

	llmResp := `{
  "synthesize": [
    {
      "path": "kb/test/synth.md",
      "title": "Synthesized insight",
      "body": "Combined understanding.",
      "domain": ["testing"],
      "confidence": 0.9,
      "entities": [],
      "refs": ["kb/test/a.md", "kb/test/b.md"]
    }
  ],
  "retract": ["kb/test/a.md"]
}`
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(llmResp, nil)

	// Write synthesized fact
	var synthWritten bool
	gs.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg, operation string) (string, string, error) {
		synthWritten = true
		return "deadbeef", "blob_synth", nil
	})
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	idx.EXPECT().GraphAddDerivedFrom(gomock.Any(), gomock.Any()).Return(nil)

	// Delete forgotten fact
	gs.EXPECT().DeleteFile("kb/test/a.md", gomock.Any(), gomock.Any()).Return("deadbeef2", nil)
	idx.EXPECT().Delete("kb/test/a.md").Return(nil)

	// Tags per operation (subsume for new fact, retract for deleted)


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
		t.Errorf("expected kb/test/synth.md to be written")
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

func TestRunDistillRetryOnPassive(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)
	adapter.EXPECT().Model().Return("qwen3:8b").AnyTimes()

	searchResults := []store.SearchResult{
		testSearchResult("kb/test/a.md", "A fact", "A body."),
		testSearchResult("kb/test/b.md", "B fact", "B body."),
	}
	idx.EXPECT().Search(gomock.Any()).Return(searchResults, nil)
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"kb/test/a.md", "kb/test/b.md"}},
	}, nil)

	// First call: passive (echoes input path, no forget)
	passiveResp := `{"synthesize": [{"path": "kb/test/a.md", "title": "A", "body": "A", "domain": [], "confidence": 0.8, "entities": [], "refs": []}], "retract": []}`
	// Second call (retry): active (new synthesized fact + forget)
	activeResp := `{
  "synthesize": [{"path": "kb/test/synth.md", "title": "Insight", "body": "Combined.", "domain": ["testing"], "confidence": 0.9, "entities": [], "refs": ["kb/test/a.md", "kb/test/b.md"]}],
  "retract": ["kb/test/a.md"]
}`

	gomock.InOrder(
		adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(passiveResp, nil),
		adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(activeResp, nil),
	)

	// Write synthesized fact
	gs.EXPECT().WriteFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("deadbeef", "blob_synth", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	idx.EXPECT().GraphAddDerivedFrom(gomock.Any(), gomock.Any()).Return(nil)

	// Delete forgotten fact
	gs.EXPECT().DeleteFile("kb/test/a.md", gomock.Any(), gomock.Any()).Return("deadbeef2", nil)
	idx.EXPECT().Delete("kb/test/a.md").Return(nil)

	// Tags per operation


	recipe := Recipe{
		Name:  "distill-retry",
		Steps: []RecipeStep{{Mode: "distill"}},
	}

	var retrySeen bool
	err := Run(context.Background(), gs, idx, nil, adapter, recipe, func(e ProgressEvent) {
		if e.Phase == "retry" {
			retrySeen = true
		}
	})
	if err != nil {
		t.Fatalf("Run distill retry: %v", err)
	}
	if !retrySeen {
		t.Error("expected 'retry' phase event from distill passive detection")
	}
}

func TestRunNilProgress(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)
	adapter.EXPECT().Model().Return("claude-sonnet-4-20250514").AnyTimes()

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
