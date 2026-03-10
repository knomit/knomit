package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// capturingIndex wraps mockIndex and records the last SearchQuery received.
type capturingIndex struct {
	mockIndex
	lastQuery SearchQuery
}

func (c *capturingIndex) Search(q SearchQuery) ([]SearchResult, error) {
	c.lastQuery = q
	return c.results, c.searchErr
}

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

func TestQueryDomainFilter(t *testing.T) {
	store := newMockStore()
	idx := &capturingIndex{
		mockIndex: mockIndex{
			results: []SearchResult{
				{FactRecord: FactRecord{Path: "know/foo.md", Title: "Foo", Body: "body", Domain: []string{"infra"}, Refs: []string{}}},
			},
		},
	}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"domain": []interface{}{"infra"},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	// Verify the domain filter was forwarded to the index.
	if len(idx.lastQuery.Domain) != 1 || idx.lastQuery.Domain[0] != "infra" {
		t.Fatalf("expected Domain=[infra] in query, got: %v", idx.lastQuery.Domain)
	}
}

func TestQueryEntityFilter(t *testing.T) {
	store := newMockStore()
	idx := &capturingIndex{
		mockIndex: mockIndex{
			results: []SearchResult{
				{FactRecord: FactRecord{Path: "know/bar.md", Title: "Bar", Body: "body", Entities: []string{"db"}, Refs: []string{}}},
			},
		},
	}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"entities": []interface{}{"db"},
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	if len(idx.lastQuery.Entities) != 1 || idx.lastQuery.Entities[0] != "db" {
		t.Fatalf("expected Entities=[db] in query, got: %v", idx.lastQuery.Entities)
	}
}

func TestQueryPathPrefixFilter(t *testing.T) {
	store := newMockStore()
	idx := &capturingIndex{
		mockIndex: mockIndex{
			results: []SearchResult{
				{FactRecord: FactRecord{Path: "know/ops/deploy.md", Title: "Deploy", Body: "body", Refs: []string{}}},
			},
		},
	}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"path": "know/ops",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	if idx.lastQuery.Path != "know/ops" {
		t.Fatalf("expected Path=know/ops in query, got: %q", idx.lastQuery.Path)
	}
}

func TestQueryMinConfidenceFilter(t *testing.T) {
	store := newMockStore()
	idx := &capturingIndex{
		mockIndex: mockIndex{
			results: []SearchResult{
				{FactRecord: FactRecord{Path: "know/sure.md", Title: "Sure", Body: "body", Confidence: 0.95, Refs: []string{}}},
			},
		},
	}
	handler := QueryHandler(store, idx)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"min_confidence": 0.9,
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	if idx.lastQuery.MinConfidence != 0.9 {
		t.Fatalf("expected MinConfidence=0.9 in query, got: %v", idx.lastQuery.MinConfidence)
	}
}
