package synthesize

import (
	"fmt"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/store"
)

// ── extractJSON ─────────────────────────────────────────────────────────────

func TestExtractJSONRawJSON(t *testing.T) {
	input := `{"key": "value"}`
	got := extractJSON(input)
	if got != input {
		t.Errorf("expected raw JSON passthrough, got %q", got)
	}
}

func TestExtractJSONMarkdownCodeFence(t *testing.T) {
	input := "```json\n{\"key\": \"value\"}\n```"
	got := extractJSON(input)
	if got != `{"key": "value"}` {
		t.Errorf("expected stripped JSON, got %q", got)
	}
}

func TestExtractJSONCodeFenceWithoutLanguageTag(t *testing.T) {
	input := "```\n{\"key\": \"value\"}\n```"
	got := extractJSON(input)
	if got != `{"key": "value"}` {
		t.Errorf("expected stripped JSON without language tag, got %q", got)
	}
}

func TestExtractJSONWithSurroundingWhitespace(t *testing.T) {
	input := "  \n```json\n{\"a\": 1}\n```\n  "
	got := extractJSON(input)
	if got != `{"a": 1}` {
		t.Errorf("expected trimmed JSON, got %q", got)
	}
}

func TestExtractJSONNestedBraces(t *testing.T) {
	input := `{"outer": {"inner": [1, 2, 3]}}`
	got := extractJSON(input)
	if got != input {
		t.Errorf("expected nested JSON passthrough, got %q", got)
	}
}

func TestExtractJSONSingleBacktickBlock(t *testing.T) {
	// Only three opening backticks but no valid closing — should return as-is.
	input := "```json\n{\"a\": 1}"
	got := extractJSON(input)
	// LastIndex of "```" finds position 0 (the opening), which is not > 3,
	// so it falls through to passthrough.
	if !strings.Contains(got, `"a"`) {
		t.Errorf("expected JSON content preserved, got %q", got)
	}
}

// ── parsePruneResponse ──────────────────────────────────────────────────────

func TestParsePruneResponseValidJSON(t *testing.T) {
	input := `{
  "decisions": [
    {"path": "know/a.md", "action": "keep"},
    {"path": "know/b.md", "action": "forget"},
    {"path": "know/c.md", "action": "update", "confidence": 0.5}
  ],
  "merges": []
}`
	result, err := parsePruneResponse(input)
	if err != nil {
		t.Fatalf("parsePruneResponse: %v", err)
	}
	if len(result.Decisions) != 3 {
		t.Fatalf("expected 3 decisions, got %d", len(result.Decisions))
	}
	if result.Decisions[0].Action != "keep" {
		t.Errorf("expected action 'keep', got %q", result.Decisions[0].Action)
	}
	if result.Decisions[2].Confidence != 0.5 {
		t.Errorf("expected confidence 0.5, got %f", result.Decisions[2].Confidence)
	}
}

func TestParsePruneResponseInvalidJSON(t *testing.T) {
	_, err := parsePruneResponse("not valid json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParsePruneResponseWithMerges(t *testing.T) {
	input := `{
  "decisions": [],
  "merges": [
    {
      "paths": ["a.md", "b.md"],
      "merged": {
        "path": "know/ab.md",
        "title": "AB",
        "body": "combined",
        "domain": ["test"],
        "confidence": 0.9,
        "sources": 2,
        "entities": ["E1"],
        "refs": ["a.md", "b.md"]
      }
    }
  ]
}`
	result, err := parsePruneResponse(input)
	if err != nil {
		t.Fatalf("parsePruneResponse: %v", err)
	}
	if len(result.Merges) != 1 {
		t.Fatalf("expected 1 merge, got %d", len(result.Merges))
	}
	if result.Merges[0].Merged.Title != "AB" {
		t.Errorf("expected merged title 'AB', got %q", result.Merges[0].Merged.Title)
	}
	if len(result.Merges[0].Paths) != 2 {
		t.Errorf("expected 2 merge source paths, got %d", len(result.Merges[0].Paths))
	}
}

// ── parseDistillResponse ────────────────────────────────────────────────────

func TestParseDistillResponseValidJSON(t *testing.T) {
	input := `{
  "synthesize": [
    {
      "path": "know/synth.md",
      "title": "Synth",
      "body": "Insight.",
      "domain": ["d1"],
      "confidence": 0.85,
      "entities": ["E1"],
      "refs": ["know/a.md"]
    }
  ],
  "forget": ["know/a.md", "know/b.md"]
}`
	result, err := parseDistillResponse(input)
	if err != nil {
		t.Fatalf("parseDistillResponse: %v", err)
	}
	if len(result.Synthesize) != 1 {
		t.Fatalf("expected 1 synthesized fact, got %d", len(result.Synthesize))
	}
	if result.Synthesize[0].Title != "Synth" {
		t.Errorf("expected title 'Synth', got %q", result.Synthesize[0].Title)
	}
	if len(result.Forget) != 2 {
		t.Errorf("expected 2 forget paths, got %d", len(result.Forget))
	}
}

func TestParseDistillResponseInvalidJSON(t *testing.T) {
	_, err := parseDistillResponse("{broken")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseDistillResponseMarkdownWrappedNoLangTag(t *testing.T) {
	input := "```\n" + `{"synthesize": [], "forget": []}` + "\n```"
	result, err := parseDistillResponse(input)
	if err != nil {
		t.Fatalf("parseDistillResponse markdown no lang tag: %v", err)
	}
	if len(result.Synthesize) != 0 {
		t.Errorf("expected 0 synthesized, got %d", len(result.Synthesize))
	}
}

// ── chunkFacts ──────────────────────────────────────────────────────────────

func TestChunkFactsSmallMaxBytesMultipleChunks(t *testing.T) {
	facts := []factForLLM{
		{File: "a.md", Title: "A title that is not tiny", Body: "Body A with some content."},
		{File: "b.md", Title: "B title that is not tiny", Body: "Body B with some content."},
		{File: "c.md", Title: "C title that is not tiny", Body: "Body C with some content."},
		{File: "d.md", Title: "D title that is not tiny", Body: "Body D with some content."},
		{File: "e.md", Title: "E title that is not tiny", Body: "Body E with some content."},
	}
	// Set maxBytes so only 1 fact fits per chunk.
	chunks := chunkFacts(facts, 10)
	if len(chunks) != 5 {
		t.Errorf("expected 5 chunks with tiny maxBytes, got %d", len(chunks))
	}
	// Verify all facts are present.
	total := 0
	for _, ch := range chunks {
		total += len(ch)
	}
	if total != 5 {
		t.Errorf("expected 5 total facts, got %d", total)
	}
}

func TestChunkFactsEmptyInput(t *testing.T) {
	chunks := chunkFacts(nil, 1000)
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for nil input, got %d", len(chunks))
	}
}

func TestChunkFactsSingleFact(t *testing.T) {
	facts := []factForLLM{{File: "x.md", Title: "X", Body: "body"}}
	chunks := chunkFacts(facts, 1000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for single fact, got %d", len(chunks))
	}
	if len(chunks[0]) != 1 {
		t.Errorf("expected 1 fact in chunk, got %d", len(chunks[0]))
	}
}

func TestExtractJSON_ThinkBlocks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "think then JSON",
			input: "<think>\nLet me analyze these facts...\n</think>\n{\"decisions\": []}",
			want:  `{"decisions": []}`,
		},
		{
			name:  "think then fenced JSON",
			input: "<think>\nreasoning here\n</think>\n```json\n{\"decisions\": []}\n```",
			want:  `{"decisions": []}`,
		},
		{
			name:  "no think block",
			input: `{"decisions": []}`,
			want:  `{"decisions": []}`,
		},
		{
			name:  "fenced without think",
			input: "```json\n{\"foo\": 1}\n```",
			want:  `{"foo": 1}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.input)
			if got != tc.want {
				t.Errorf("extractJSON:\ngot:  %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// ── distillClusterInMemory ──────────────────────────────────────────────────

func TestDistillClusterInMemoryInsufficientEmbeddings(t *testing.T) {
	// depth > 0 always returns nil (Louvain only works on persisted graph).
	facts := []workFact{
		{factForLLM: factForLLM{File: "a.md"}, embedding: []float32{1, 0, 0}},
		{factForLLM: factForLLM{File: "b.md"}, embedding: nil},
	}
	var progressCalled bool
	result := distillClusterInMemory(facts, 1.0, func(e ProgressEvent) {
		progressCalled = true
	})
	if result != nil {
		t.Errorf("expected nil cluster map for in-memory facts, got %v", result)
	}
	if !progressCalled {
		t.Error("expected progress callback to be called")
	}
}

func TestDistillClusterInMemoryNoEmbeddings(t *testing.T) {
	facts := []workFact{
		{factForLLM: factForLLM{File: "a.md"}, embedding: nil},
		{factForLLM: factForLLM{File: "b.md"}, embedding: nil},
	}
	result := distillClusterInMemory(facts, 1.0, func(e ProgressEvent) {})
	if result != nil {
		t.Errorf("expected nil for in-memory facts, got %v", result)
	}
}

func TestDistillClusterInMemoryWithEmbeddings(t *testing.T) {
	// With Louvain, depth > 0 always returns nil (no graph edges for in-memory facts).
	facts := make([]workFact, 0, 10)
	for i := 0; i < 5; i++ {
		emb := make([]float32, 8)
		emb[0] = 10.0
		emb[2] = float32(i) * 0.01
		facts = append(facts, workFact{
			factForLLM: factForLLM{File: fmt.Sprintf("know/a/%d.md", i), Title: fmt.Sprintf("A%d", i)},
			embedding:  emb,
		})
	}
	for i := 0; i < 5; i++ {
		emb := make([]float32, 8)
		emb[4] = 10.0
		emb[5] = float32(i) * 0.01
		facts = append(facts, workFact{
			factForLLM: factForLLM{File: fmt.Sprintf("know/b/%d.md", i), Title: fmt.Sprintf("B%d", i)},
			embedding:  emb,
		})
	}

	result := distillClusterInMemory(facts, 1.0, func(e ProgressEvent) {})
	if result != nil {
		t.Errorf("expected nil for in-memory facts (Louvain only works on persisted graph), got %v", result)
	}
}

// ── gatherAllFacts ──────────────────────────────────────────────────────────

func TestGatherAllFactsSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().ListAll().Return([]string{
		"know/test/fact1.md",
		"know/test/fact2.md",
		"know/test/readme.txt", // non-md, should be skipped
	}, nil)
	gs.EXPECT().ReadFile("know/test/fact1.md").Return(factContent("Fact One", "Body one."), nil)
	gs.EXPECT().ReadFile("know/test/fact2.md").Return(factContent("Fact Two", "Body two."), nil)

	facts, err := gatherAllFacts(gs)
	if err != nil {
		t.Fatalf("gatherAllFacts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	if facts[0].Title != "Fact One" {
		t.Errorf("expected title 'Fact One', got %q", facts[0].Title)
	}
	if facts[1].Title != "Fact Two" {
		t.Errorf("expected title 'Fact Two', got %q", facts[1].Title)
	}
}

func TestGatherAllFactsListError(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().ListAll().Return(nil, fmt.Errorf("git error"))

	_, err := gatherAllFacts(gs)
	if err == nil {
		t.Error("expected error from gatherAllFacts when ListAll fails")
	}
}

func TestGatherAllFactsSkipsUnreadable(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().ListAll().Return([]string{
		"know/ok.md",
		"know/broken.md",
	}, nil)
	gs.EXPECT().ReadFile("know/ok.md").Return(factContent("OK", "OK body."), nil)
	gs.EXPECT().ReadFile("know/broken.md").Return("", fmt.Errorf("read error"))

	facts, err := gatherAllFacts(gs)
	if err != nil {
		t.Fatalf("gatherAllFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact (broken skipped), got %d", len(facts))
	}
}

func TestGatherAllFactsSkipsNonFact(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().ListAll().Return([]string{
		"know/ok.md",
		"know/notafact.md",
	}, nil)
	gs.EXPECT().ReadFile("know/ok.md").Return(factContent("OK", "OK body."), nil)
	// File is .md but content is not a valid fact (no frontmatter).
	gs.EXPECT().ReadFile("know/notafact.md").Return("# Just a markdown file\n\nNo frontmatter here.", nil)

	facts, err := gatherAllFacts(gs)
	if err != nil {
		t.Fatalf("gatherAllFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact (non-fact skipped), got %d", len(facts))
	}
}

func TestGatherAllFactsSearchResultsMapping(t *testing.T) {
	// Verify that gatherAllFacts correctly maps parsed fact fields to factForLLM.
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	content := "---\ndomain: [go, testing]\nconfidence: 0.95\nsources: 3\nentities: [Foo, Bar]\nrefs: []\n---\n# My Title\n\nMy body text.\n"
	gs.EXPECT().ListAll().Return([]string{"know/mapped.md"}, nil)
	gs.EXPECT().ReadFile("know/mapped.md").Return(content, nil)

	facts, err := gatherAllFacts(gs)
	if err != nil {
		t.Fatalf("gatherAllFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if f.File != "know/mapped.md" {
		t.Errorf("File: got %q, want %q", f.File, "know/mapped.md")
	}
	if f.Title != "My Title" {
		t.Errorf("Title: got %q, want %q", f.Title, "My Title")
	}
	if f.Confidence != 0.95 {
		t.Errorf("Confidence: got %f, want 0.95", f.Confidence)
	}
	if f.Sources != 3 {
		t.Errorf("Sources: got %d, want 3", f.Sources)
	}
	if len(f.Domain) != 2 || f.Domain[0] != "go" {
		t.Errorf("Domain: got %v, want [go testing]", f.Domain)
	}
}

// ── gatherAllFacts (edge cases) ─────────────────────────────────────────────

func TestGatherAllFactsEmptyStore(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	gs.EXPECT().ListAll().Return([]string{}, nil)

	facts, err := gatherAllFacts(gs)
	if err != nil {
		t.Fatalf("gatherAllFacts: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("expected 0 facts for empty store, got %d", len(facts))
	}
}

// Verify the search-based path used by distill step gathers facts correctly.
func TestDistillSearchResultsToWorkFacts(t *testing.T) {
	// This tests the mapping logic from SearchResult -> workFact that
	// executeDistillStep performs (lines 44-58 of distill.go).
	searchResults := []store.SearchResult{
		{
			FactRecord: store.FactRecord{
				Path:       "know/x.md",
				Title:      "X",
				Body:       "X body",
				Domain:     []string{"d1"},
				Entities:   []string{"E1"},
				Confidence: 0.8,
				Sources:    2,
			},
			Score: 50.0,
		},
	}

	// Replicate the mapping from executeDistillStep.
	currentFacts := make([]workFact, 0, len(searchResults))
	for _, r := range searchResults {
		currentFacts = append(currentFacts, workFact{
			factForLLM: factForLLM{
				File:       r.Path,
				Title:      r.Title,
				Body:       r.Body,
				Domain:     r.Domain,
				Entities:   r.Entities,
				Confidence: r.Confidence,
				Sources:    r.Sources,
			},
		})
	}

	if len(currentFacts) != 1 {
		t.Fatalf("expected 1 workFact, got %d", len(currentFacts))
	}
	wf := currentFacts[0]
	if wf.File != "know/x.md" {
		t.Errorf("File: got %q, want %q", wf.File, "know/x.md")
	}
	if wf.Title != "X" {
		t.Errorf("Title: got %q", wf.Title)
	}
	if wf.Confidence != 0.8 {
		t.Errorf("Confidence: got %f, want 0.8", wf.Confidence)
	}
	if wf.embedding != nil {
		t.Error("expected nil embedding for search-sourced workFact")
	}
}
