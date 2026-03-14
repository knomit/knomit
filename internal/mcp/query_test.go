package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/mock/gomock"
)

func TestQueryReturnsResults(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)
	idx.EXPECT().Search(gomock.Any()).Return([]SearchResult{
		{
			FactWithBody: FactWithBody{
				FactRecord: FactRecord{
					Path:       "know/foo.md",
					Title:      "Foo",
					BlobHash:   "blob_foo",
					Domain:     []string{"testing"},
					Entities:   []string{"foo"},
					Confidence: 0.9,
					Sources:    1,
					Refs:       []string{},
					CommitHash: "abc123",
				},
				Body: "Foo body.",
			},
			Score: 95.0,
		},
	}, nil)

	handler := QueryHandler(gs, idx)

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
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)

	handler := QueryHandler(gs, idx)

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
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)
	idx.EXPECT().Search(gomock.Any()).Return(nil, nil)

	handler := QueryHandler(gs, idx)

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

// helper to build a SearchResult with body for test mocks
func testSearchResult(path, title, body string, extras ...func(*SearchResult)) SearchResult {
	r := SearchResult{
		FactWithBody: FactWithBody{
			FactRecord: FactRecord{Path: path, Title: title, Refs: []string{}},
			Body:       body,
		},
	}
	for _, fn := range extras {
		fn(&r)
	}
	return r
}

func TestQueryDomainFilter(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var lastQuery SearchQuery

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)
	idx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		r := testSearchResult("know/foo.md", "Foo", "body")
		r.Domain = []string{"infra"}
		return []SearchResult{r}, nil
	})

	handler := QueryHandler(gs, idx)

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

	if len(lastQuery.Domain) != 1 || lastQuery.Domain[0] != "infra" {
		t.Fatalf("expected Domain=[infra] in query, got: %v", lastQuery.Domain)
	}
}

func TestQueryEntityFilter(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var lastQuery SearchQuery

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)
	idx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		r := testSearchResult("know/bar.md", "Bar", "body")
		r.Entities = []string{"db"}
		return []SearchResult{r}, nil
	})

	handler := QueryHandler(gs, idx)

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

	if len(lastQuery.Entities) != 1 || lastQuery.Entities[0] != "db" {
		t.Fatalf("expected Entities=[db] in query, got: %v", lastQuery.Entities)
	}
}

func TestQueryPathPrefixFilter(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var lastQuery SearchQuery

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)
	idx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		return []SearchResult{testSearchResult("know/ops/deploy.md", "Deploy", "body")}, nil
	})

	handler := QueryHandler(gs, idx)

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

	if lastQuery.Path != "know/ops" {
		t.Fatalf("expected Path=know/ops in query, got: %q", lastQuery.Path)
	}
}

func TestQueryMinConfidenceFilter(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var lastQuery SearchQuery

	gs.EXPECT().Sync(nil).Return(SyncResult{}, nil)
	idx.EXPECT().Sync(gomock.Any()).Return(nil)
	idx.EXPECT().Search(gomock.Any()).DoAndReturn(func(q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		r := testSearchResult("know/sure.md", "Sure", "body")
		r.Confidence = 0.95
		return []SearchResult{r}, nil
	})

	handler := QueryHandler(gs, idx)

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

	if lastQuery.MinConfidence != 0.9 {
		t.Fatalf("expected MinConfidence=0.9 in query, got: %v", lastQuery.MinConfidence)
	}
}
