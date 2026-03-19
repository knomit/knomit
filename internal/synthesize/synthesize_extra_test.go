package synthesize

import (
	"strings"
	"testing"
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
    {"path": "kb/a.md", "action": "keep"},
    {"path": "kb/b.md", "action": "retract"},
    {"path": "kb/c.md", "action": "update", "confidence": 0.5}
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
        "path": "kb/ab.md",
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
      "path": "kb/synth.md",
      "title": "Synth",
      "body": "Insight.",
      "domain": ["d1"],
      "confidence": 0.85,
      "entities": ["E1"],
      "refs": ["kb/a.md"]
    }
  ],
  "retract": ["kb/a.md", "kb/b.md"]
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
	if len(result.Retract) != 2 {
		t.Errorf("expected 2 forget paths, got %d", len(result.Retract))
	}
}

func TestParseDistillResponseInvalidJSON(t *testing.T) {
	_, err := parseDistillResponse("{broken")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseDistillResponseMarkdownWrappedNoLangTag(t *testing.T) {
	input := "```\n" + `{"synthesize": [], "retract": []}` + "\n```"
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

// ── validatePrunePaths ──────────────────────────────────────────────────────

func TestValidatePrunePaths_AllValid(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md", "kb/c.md"}
	result := PruneResult{
		Decisions: []PruneDecision{
			{Path: "kb/a.md", Action: "keep"},
			{Path: "kb/b.md", Action: "retract"},
			{Path: "kb/c.md", Action: "update", Confidence: 0.5},
		},
	}
	if err := validatePrunePaths(result, inputPaths); err != nil {
		t.Errorf("expected no error for valid paths, got: %v", err)
	}
}

func TestValidatePrunePaths_BogusPath(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md"}
	result := PruneResult{
		Decisions: []PruneDecision{
			{Path: ".", Action: "keep", Confidence: 0.9},
		},
	}
	err := validatePrunePaths(result, inputPaths)
	if err == nil {
		t.Fatal("expected error for bogus path '.'")
	}
	if !strings.Contains(err.Error(), ".") {
		t.Errorf("error should mention the bogus path, got: %v", err)
	}
}

func TestValidatePrunePaths_EmptyDecisions(t *testing.T) {
	inputPaths := []string{"kb/a.md"}
	result := PruneResult{Decisions: nil}
	if err := validatePrunePaths(result, inputPaths); err != nil {
		t.Errorf("expected no error for empty decisions, got: %v", err)
	}
}

func TestValidatePrunePaths_MergeSourcesValidated(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md"}
	result := PruneResult{
		Merges: []MergeEntry{
			{
				Paths:  []string{"kb/a.md", "kb/NONEXISTENT.md"},
				Merged: mergedFact{Path: "kb/ab.md", Title: "Combined"},
			},
		},
	}
	err := validatePrunePaths(result, inputPaths)
	if err == nil {
		t.Fatal("expected error for bogus merge source path")
	}
	if !strings.Contains(err.Error(), "NONEXISTENT") {
		t.Errorf("error should mention the bogus merge source, got: %v", err)
	}
}

func TestValidatePrunePaths_UnknownAction(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md"}
	result := PruneResult{
		Decisions: []PruneDecision{
			{Path: "kb/a.md", Action: "merge", Confidence: 0.75},
		},
	}
	err := validatePrunePaths(result, inputPaths)
	if err == nil {
		t.Fatal("expected error for unknown action 'merge'")
	}
	if !strings.Contains(err.Error(), "merge") {
		t.Errorf("error should mention the unknown action, got: %v", err)
	}
}

func TestValidatePrunePaths_MergeEmptyPaths(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md"}
	// Model used wrong schema: "from"/"to" instead of "paths"/"merged" —
	// Paths unmarshals as nil, Merged as zero-value.
	result := PruneResult{
		Merges: []MergeEntry{
			{Paths: nil, Merged: mergedFact{}},
		},
	}
	err := validatePrunePaths(result, inputPaths)
	if err == nil {
		t.Fatal("expected error for merge with empty paths")
	}
}

func TestValidatePrunePaths_MergeEmptyTitle(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md"}
	result := PruneResult{
		Merges: []MergeEntry{
			{
				Paths:  []string{"kb/a.md", "kb/b.md"},
				Merged: mergedFact{Path: "kb/ab.md", Title: ""},
			},
		},
	}
	err := validatePrunePaths(result, inputPaths)
	if err == nil {
		t.Fatal("expected error for merge with empty title")
	}
}

// ── validateDistillPaths ────────────────────────────────────────────────────

func TestValidateDistillPaths_AllValid(t *testing.T) {
	inputPaths := []string{"kb/a.md", "kb/b.md"}
	result := DistillResult{
		Retract: []string{"kb/a.md"},
	}
	if err := validateDistillPaths(result, inputPaths); err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestValidateDistillPaths_BogusForgetPath(t *testing.T) {
	inputPaths := []string{"kb/a.md"}
	result := DistillResult{
		Retract: []string{"kb/FAKE.md"},
	}
	err := validateDistillPaths(result, inputPaths)
	if err == nil {
		t.Fatal("expected error for bogus forget path")
	}
	if !strings.Contains(err.Error(), "FAKE") {
		t.Errorf("error should mention the bogus path, got: %v", err)
	}
}

// ── SearchResult to workFact mapping ────────────────────────────────────────

// Removed: TestDistillClusterInMemory*, TestGatherAllFacts*, TestDistillSearchResultsToWorkFacts
// (they tested deleted pipeline code)
