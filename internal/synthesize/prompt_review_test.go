package synthesize

import (
	"encoding/json"
	"strings"
	"testing"
)

func testFacts() []factForLLM {
	return []factForLLM{
		{
			File:       "kb/go/error-handling.md",
			Title:      "Go Error Handling Best Practices",
			Body:       "Always wrap errors with fmt.Errorf and %w verb.",
			Type:       "convention",
			Domain:     []string{"go", "errors"},
			Entities:   []string{"Go"},
			Confidence: 0.9,
			Sources:    3,
		},
		{
			File:       "kb/testing/table-tests.md",
			Title:      "Table-Driven Tests",
			Body:       "Use table-driven tests for comprehensive coverage.",
			Type:       "pattern",
			Domain:     []string{"testing"},
			Entities:   []string{"Go", "testing"},
			Confidence: 0.85,
			Sources:    2,
		},
	}
}

func TestRenderPruneWorkItem(t *testing.T) {
	facts := testFacts()

	wic, err := RenderPruneWorkItem(facts)
	if err != nil {
		t.Fatalf("RenderPruneWorkItem: %v", err)
	}

	// Prompt should contain fact content.
	if !strings.Contains(wic.Prompt, "Go Error Handling Best Practices") {
		t.Error("prompt missing fact title")
	}
	if !strings.Contains(wic.Prompt, "Table-Driven Tests") {
		t.Error("prompt missing second fact title")
	}

	// Prompt should include expected action words.
	for _, word := range []string{"keep", "retract"} {
		if !strings.Contains(wic.Prompt, word) {
			t.Errorf("prompt missing expected action word %q", word)
		}
	}

	// ResponseSchema must be valid JSON.
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(wic.ResponseSchema), &schema); err != nil {
		t.Fatalf("response_schema is not valid JSON: %v", err)
	}

	// Schema should describe a prune response with decisions.
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, ok := props["decisions"]; !ok {
		t.Error("schema missing decisions property")
	}
}

func TestRenderDistillWorkItem(t *testing.T) {
	facts := testFacts()

	wic, err := RenderDistillWorkItem(facts)
	if err != nil {
		t.Fatalf("RenderDistillWorkItem: %v", err)
	}

	// Prompt should contain fact content.
	if !strings.Contains(wic.Prompt, "Go Error Handling Best Practices") {
		t.Error("prompt missing fact title")
	}
	if !strings.Contains(wic.Prompt, "Table-Driven Tests") {
		t.Error("prompt missing second fact title")
	}

	// Prompt should include expected action word.
	if !strings.Contains(wic.Prompt, "synthesiz") {
		t.Error("prompt missing expected action word 'synthesiz'")
	}

	// ResponseSchema must be valid JSON.
	var schema map[string]interface{}
	if err := json.Unmarshal([]byte(wic.ResponseSchema), &schema); err != nil {
		t.Fatalf("response_schema is not valid JSON: %v", err)
	}

	// Schema should describe a distill response with synthesize.
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("schema missing properties")
	}
	if _, ok := props["synthesize"]; !ok {
		t.Error("schema missing synthesize property")
	}
}

func TestRenderPruneWorkItem_EmptyFacts(t *testing.T) {
	wic, err := RenderPruneWorkItem([]factForLLM{})
	if err != nil {
		t.Fatalf("RenderPruneWorkItem with empty facts: %v", err)
	}
	if wic.Prompt == "" {
		t.Error("expected non-empty prompt even with empty facts")
	}
}

func TestRenderDistillWorkItem_EmptyFacts(t *testing.T) {
	wic, err := RenderDistillWorkItem([]factForLLM{})
	if err != nil {
		t.Fatalf("RenderDistillWorkItem with empty facts: %v", err)
	}
	if wic.Prompt == "" {
		t.Error("expected non-empty prompt even with empty facts")
	}
}
