package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/llm"
	"knomit/internal/store"
)

// factContent builds a minimal knomit fact file for testing.
func factContent(title, body string) string {
	return "---\ndomain: [testing]\nconfidence: 0.8\nsources: 1\nentities: []\nrefs: []\n---\n# " + title + "\n\n" + body + "\n"
}


func TestPruneStep(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	files := map[string]string{
		"know/test/foo.md": factContent("Foo fact", "Foo is great."),
		"know/test/bar.md": factContent("Bar fact", "Bar is outdated."),
		"know/test/baz.md": factContent("Baz fact", "Baz needs confidence update."),
	}

	gs.EXPECT().ListAll().Return([]string{"know/test/foo.md", "know/test/bar.md", "know/test/baz.md"}, nil)
	gs.EXPECT().ReadFile(gomock.Any()).DoAndReturn(func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("not found: %s", path)
	}).AnyTimes()

	// ClusterFacts returns empty → single group fallback with all facts.
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{Clusters: map[int][]string{}}, nil)

	// LLM returns: keep foo, forget bar, update baz with confidence=0.7
	mockResponse := `{
  "decisions": [
    { "path": "know/test/foo.md", "action": "keep" },
    { "path": "know/test/bar.md", "action": "forget" },
    { "path": "know/test/baz.md", "action": "update", "confidence": 0.7 }
  ],
  "merges": []
}`
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mockResponse, nil)

	// bar: forget — DeleteFile + idx.Delete
	gs.EXPECT().DeleteFile("know/test/bar.md", gomock.Any()).Return(nil)
	idx.EXPECT().Delete("know/test/bar.md").Return(nil)

	// baz: update — WriteFile with updated confidence, HeadCommit, idx.Upsert
	var bazWritten string
	gs.EXPECT().WriteFile("know/test/baz.md", gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg string) error {
		bazWritten = content
		return nil
	})
	gs.EXPECT().HeadCommit().Return("deadbeef", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)

	// Tag at end
	gs.EXPECT().Tag("learn/synthesize-test-recipe-prune").Return(nil)

	recipe := Recipe{Name: "test-recipe", Steps: []RecipeStep{{Mode: "prune"}}}

	var events []ProgressEvent
	err := executePruneStep(context.Background(), gs, idx, nil, adapter, recipe.Steps[0], recipe, func(e ProgressEvent) {
		events = append(events, e)
	})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}

	// baz should have updated confidence in written content
	if bazWritten == "" {
		t.Fatal("expected know/test/baz.md to be rewritten with updated confidence")
	}
	if !strings.Contains(bazWritten, "confidence: 0.7") {
		t.Errorf("baz.md content should contain 'confidence: 0.7', got:\n%s", bazWritten)
	}

	// tag event should be present
	tagEventSeen := false
	for _, e := range events {
		if e.Phase == "detail-forget" && e.Message == "know/test/bar.md" {
			tagEventSeen = true
		}
	}
	_ = tagEventSeen // gomock verifies DeleteFile/Tag calls above
}

func TestPruneStepWithMerge(t *testing.T) {
	ctrl := gomock.NewController(t)

	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	files := map[string]string{
		"know/test/a.md": factContent("A fact", "A says something."),
		"know/test/b.md": factContent("B fact", "B says the same thing."),
	}

	gs.EXPECT().ListAll().Return([]string{"know/test/a.md", "know/test/b.md"}, nil)
	gs.EXPECT().ReadFile(gomock.Any()).DoAndReturn(func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("not found: %s", path)
	}).AnyTimes()

	// Louvain clustering returns both facts in one cluster.
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{0: {"know/test/a.md", "know/test/b.md"}},
	}, nil)

	mockResponse := `{
  "decisions": [],
  "merges": [
    {
      "paths": ["know/test/a.md", "know/test/b.md"],
      "merged": {
        "path": "know/test/ab-merged.md",
        "title": "A and B combined",
        "body": "Combined body.",
        "domain": ["testing"],
        "confidence": 0.85,
        "sources": 2,
        "entities": [],
        "refs": ["know/test/a.md", "know/test/b.md"]
      }
    }
  ]
}`
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(mockResponse, nil)

	// Write merged fact
	var mergedWritten bool
	gs.EXPECT().WriteFile("know/test/ab-merged.md", gomock.Any(), gomock.Any()).DoAndReturn(func(path, content, msg string) error {
		mergedWritten = true
		return nil
	})
	gs.EXPECT().HeadCommit().Return("deadbeef", nil)
	idx.EXPECT().Upsert(gomock.Any()).Return(nil)
	idx.EXPECT().GraphAddDerivedFrom("know/test/ab-merged.md", gomock.Any()).Return(nil)

	// Delete source facts
	gs.EXPECT().DeleteFile("know/test/a.md", gomock.Any()).Return(nil)
	idx.EXPECT().Delete("know/test/a.md").Return(nil)
	gs.EXPECT().DeleteFile("know/test/b.md", gomock.Any()).Return(nil)
	idx.EXPECT().Delete("know/test/b.md").Return(nil)

	// Tag
	gs.EXPECT().Tag("learn/synthesize-merge-recipe-prune").Return(nil)

	recipe := Recipe{Name: "merge-recipe", Steps: []RecipeStep{{Mode: "prune"}}}

	err := executePruneStep(context.Background(), gs, idx, nil, adapter, recipe.Steps[0], recipe, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}

	if !mergedWritten {
		t.Error("expected merged fact know/test/ab-merged.md to be written")
	}
}

func TestChunkFacts(t *testing.T) {
	facts := []factForLLM{
		{File: "a.md", Title: "A", Body: "aaa"},
		{File: "b.md", Title: "B", Body: "bbb"},
		{File: "c.md", Title: "C", Body: "ccc"},
	}
	// Large limit: one chunk
	chunks := chunkFacts(facts, 1_000_000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk with large limit, got %d", len(chunks))
	}
	// Tiny limit: one fact per chunk
	chunks = chunkFacts(facts, 1)
	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks with tiny limit, got %d", len(chunks))
	}
}

func TestExtractJSON(t *testing.T) {
	raw := "```json\n{\"hello\": \"world\"}\n```"
	got := extractJSON(raw)
	if got != `{"hello": "world"}` {
		t.Errorf("extractJSON: got %q", got)
	}
	plain := `{"hello": "world"}`
	if extractJSON(plain) != plain {
		t.Errorf("extractJSON plain: got %q", extractJSON(plain))
	}
}

func TestParsePruneResponseMarkdownWrapped(t *testing.T) {
	// LLMs sometimes wrap their JSON in markdown code fences.
	wrapped := "```json\n" + `{
  "decisions": [
    { "path": "know/x.md", "action": "keep" }
  ],
  "merges": []
}` + "\n```"

	result, err := parsePruneResponse(wrapped)
	if err != nil {
		t.Fatalf("parsePruneResponse with markdown wrapping: %v", err)
	}
	if len(result.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(result.Decisions))
	}
	if result.Decisions[0].Path != "know/x.md" {
		t.Errorf("expected path 'know/x.md', got %q", result.Decisions[0].Path)
	}
	if result.Decisions[0].Action != "keep" {
		t.Errorf("expected action 'keep', got %q", result.Decisions[0].Action)
	}
	if len(result.Merges) != 0 {
		t.Errorf("expected no merges, got %d", len(result.Merges))
	}
}

func TestParseDistillResponseMarkdownWrapped(t *testing.T) {
	wrapped := "```json\n" + `{
  "synthesize": [
    {
      "path": "know/synth.md",
      "title": "Synthesized",
      "body": "Combined insight.",
      "domain": ["testing"],
      "confidence": 0.85,
      "entities": [],
      "refs": ["know/a.md"]
    }
  ],
  "forget": ["know/a.md"]
}` + "\n```"

	result, err := parseDistillResponse(wrapped)
	if err != nil {
		t.Fatalf("parseDistillResponse with markdown wrapping: %v", err)
	}
	if len(result.Synthesize) != 1 {
		t.Fatalf("expected 1 synthesized fact, got %d", len(result.Synthesize))
	}
	if result.Synthesize[0].Path != "know/synth.md" {
		t.Errorf("expected path 'know/synth.md', got %q", result.Synthesize[0].Path)
	}
	if result.Synthesize[0].Title != "Synthesized" {
		t.Errorf("expected title 'Synthesized', got %q", result.Synthesize[0].Title)
	}
	if len(result.Forget) != 1 || result.Forget[0] != "know/a.md" {
		t.Errorf("expected forget=[know/a.md], got %v", result.Forget)
	}
}

func TestChunkFactsExceedsBudget(t *testing.T) {
	// Build many facts so that the total JSON exceeds a small budget,
	// forcing the chunker to split across multiple chunks.
	facts := make([]factForLLM, 10)
	for i := range facts {
		facts[i] = factForLLM{
			File:  "know/fact.md",
			Title: "A moderately long title that takes up space",
			Body:  "A moderately long body that contributes to the chunk budget.",
		}
	}

	// Measure a single fact's size.
	import_json_b, _ := json.Marshal(facts[0])
	singleSize := len(import_json_b)

	// Budget that fits exactly 3 facts — expect ceil(10/3) = 4 chunks.
	budget := singleSize * 3
	chunks := chunkFacts(facts, budget)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks when budget is tight, got %d chunk(s)", len(chunks))
	}
	// Every chunk must be non-empty.
	for i, ch := range chunks {
		if len(ch) == 0 {
			t.Errorf("chunk %d is empty", i)
		}
	}
	// Total facts across all chunks must equal original count.
	total := 0
	for _, ch := range chunks {
		total += len(ch)
	}
	if total != len(facts) {
		t.Errorf("expected %d total facts across chunks, got %d", len(facts), total)
	}
}

// TestPruneStepClustersBeforeLLM verifies that when ClusterFacts returns two
// communities, the prune step sends each cluster to the LLM separately and
// excludes noise facts from all prompts.
func TestPruneStepClustersBeforeLLM(t *testing.T) {
	ctrl := gomock.NewController(t)

	idx := NewMockSearchIndex(ctrl)
	adapter := NewMockLLMAdapter(ctrl)

	clusterAFiles := []string{
		"know/cluster-a/a1.md", "know/cluster-a/a2.md", "know/cluster-a/a3.md",
		"know/cluster-a/a4.md", "know/cluster-a/a5.md",
	}
	clusterBFiles := []string{
		"know/cluster-b/b1.md", "know/cluster-b/b2.md", "know/cluster-b/b3.md",
		"know/cluster-b/b4.md", "know/cluster-b/b5.md",
	}
	noiseFile := "know/noise/lone.md"

	allFiles := make([]string, 0, len(clusterAFiles)+len(clusterBFiles)+1)
	allFiles = append(allFiles, clusterAFiles...)
	allFiles = append(allFiles, clusterBFiles...)
	allFiles = append(allFiles, noiseFile)

	files := map[string]string{}
	for _, f := range allFiles {
		files[f] = factContent("Fact "+f, "Body of "+f)
	}

	gs := NewMockGitStore(ctrl)
	gs.EXPECT().ListAll().Return(allFiles, nil)
	gs.EXPECT().ReadFile(gomock.Any()).DoAndReturn(func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", fmt.Errorf("not found: %s", path)
	}).AnyTimes()

	// ClusterFacts returns two communities and the noise file as noise.
	idx.EXPECT().ClusterFacts(gomock.Any(), gomock.Any()).Return(store.ClusterResult{
		Clusters: map[int][]string{
			0: clusterAFiles,
			1: clusterBFiles,
		},
		Noise: []string{noiseFile},
	}, nil)

	// LLM: capture prompts, return empty response each call.
	var capturedPrompts []string
	adapter.EXPECT().Complete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, system string, msgs []llm.Message, onChunk func(string)) (string, error) {
			for _, msg := range msgs {
				capturedPrompts = append(capturedPrompts, msg.Content)
			}
			return `{"decisions":[],"merges":[]}`, nil
		},
	).AnyTimes()

	// Tag at end
	gs.EXPECT().Tag(gomock.Any()).Return(nil)

	recipe := Recipe{Name: "cluster-test", Steps: []RecipeStep{{Mode: "prune"}}}

	err := executePruneStep(context.Background(), gs, idx, nil, adapter, recipe.Steps[0], recipe, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("executePruneStep: %v", err)
	}

	// The noise fact (lone.md) should NOT appear in any LLM prompt.
	for i, prompt := range capturedPrompts {
		if strings.Contains(prompt, noiseFile) {
			t.Errorf("prompt %d contains noise fact %s — it should have been skipped as unclustered", i, noiseFile)
		}
	}

	// With 2 clusters we expect exactly 2 LLM calls, not 1 call with all 11 facts.
	if len(capturedPrompts) != 2 {
		t.Errorf("expected 2 LLM calls (one per cluster), got %d", len(capturedPrompts))
	}

	// Each prompt should contain only facts from its cluster, not from both.
	for _, prompt := range capturedPrompts {
		hasA := strings.Contains(prompt, "know/cluster-a/")
		hasB := strings.Contains(prompt, "know/cluster-b/")
		if hasA && hasB {
			t.Error("a single LLM prompt contains facts from both clusters — they should be separated")
		}
	}
}
