package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func TestQueryReturnsResults(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{
		results: []SearchResult{
			{
				FactRecord: FactRecord{
					Path:       "know/foo.md",
					Title:      "Foo",
					Body:       "Foo body.",
					Domain:     []string{"testing"},
					Entities:   []string{"foo"},
					Confidence: 0.9,
					Sources:    1,
					Refs:       []string{},
					CommitHash: "abc123",
				},
				Score: 95.0,
			},
		},
	}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"text": "foo",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, text)
	}
	facts, ok := resp["facts"].([]interface{})
	if !ok || len(facts) != 1 {
		t.Fatalf("expected 1 fact, got: %v", resp["facts"])
	}
}

func TestQueryRequiresFilter(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for empty query")
	}
}

func TestQueryEmptyResults(t *testing.T) {
	store := newMockStore()
	idx := &mockIndex{results: nil}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"text": "nonexistent",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}

	text := getResultText(t, result)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	facts, _ := resp["facts"].([]interface{})
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts, got %d", len(facts))
	}
}
