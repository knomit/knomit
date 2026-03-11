package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestLearnWritesFacts(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := LearnHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "test-moment",
		"facts": []interface{}{
			map[string]interface{}{
				"path":       "test/foo",
				"title":      "Test Fact",
				"body":       "Some body text.",
				"domain":     []interface{}{"testing"},
				"confidence": 0.9,
				"sources":    1,
				"entities":   []interface{}{"foo"},
				"refs":       []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned tool error: %v", result.Content)
	}

	// Verify file was written with normalized path.
	expectedPath := "know/test/foo.md"
	if _, ok := store.written[expectedPath]; !ok {
		t.Fatalf("expected file %q to be written; written: %v", expectedPath, store.written)
	}

	// Verify the file content parses correctly.
	content := store.written[expectedPath]
	fact, err := ParseFact(expectedPath, content)
	if err != nil {
		t.Fatalf("written file does not parse: %v", err)
	}
	if fact.Title != "Test Fact" {
		t.Fatalf("title: got %q want %q", fact.Title, "Test Fact")
	}

	// Verify result JSON has moment_tag.
	textContent := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(textContent), &resp); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, textContent)
	}
	tag, _ := resp["moment_tag"].(string)
	if !strings.HasPrefix(tag, "learn/test-moment") {
		t.Fatalf("moment_tag: got %q want prefix learn/test-moment", tag)
	}

	// Verify index was updated.
	if len(idx.upserted) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(idx.upserted))
	}
	if idx.upserted[0].Path != expectedPath {
		t.Fatalf("upserted path: got %q want %q", idx.upserted[0].Path, expectedPath)
	}

	// Verify tag was set.
	if len(store.tags) == 0 {
		t.Fatal("expected tag to be set")
	}
}

func TestLearnNormalizesPath(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := LearnHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "path-test",
		"facts": []interface{}{
			map[string]interface{}{
				"path":       "know/already/normalized.md",
				"title":      "Normalized",
				"body":       "",
				"domain":     []interface{}{},
				"confidence": 0.5,
				"sources":    0,
				"entities":   []interface{}{},
				"refs":       []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Already-normalized path should not be doubled.
	if _, ok := store.written["know/already/normalized.md"]; !ok {
		t.Fatalf("expected know/already/normalized.md in written, got: %v", store.written)
	}
	if _, ok := store.written["know/know/already/normalized.md"]; ok {
		t.Fatal("path was incorrectly double-prefixed")
	}
}

func TestLearnRequiresMomentName(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := LearnHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"facts": []interface{}{},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing moment_name")
	}
}

func TestLearnMultipleFacts(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := LearnHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"moment_name": "multi",
		"facts": []interface{}{
			map[string]interface{}{
				"path": "a", "title": "Fact A", "body": "A body.",
				"domain": []interface{}{}, "confidence": 0.8, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
			map[string]interface{}{
				"path": "b", "title": "Fact B", "body": "B body.",
				"domain": []interface{}{}, "confidence": 0.7, "sources": 1,
				"entities": []interface{}{}, "refs": []interface{}{},
			},
		},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	if _, ok := store.written["know/a.md"]; !ok {
		t.Error("missing know/a.md")
	}
	if _, ok := store.written["know/b.md"]; !ok {
		t.Error("missing know/b.md")
	}
	if len(idx.upserted) != 2 {
		t.Fatalf("expected 2 upserts, got %d", len(idx.upserted))
	}
}

// getResultText extracts the text content from a CallToolResult.
func getResultText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	for _, c := range result.Content {
		if tc, ok := mcpgo.AsTextContent(c); ok {
			return tc.Text
		}
	}
	t.Fatal("no text content in result")
	return ""
}
