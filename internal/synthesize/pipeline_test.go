package synthesize

import (
	"context"
	"testing"
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
