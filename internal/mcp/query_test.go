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



	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]SearchResult{
		{
			FactWithBody: FactWithBody{
				FactRecord: FactRecord{
					Path:       "general/foo.md",
					Title:      "Foo",
					BlobHash:   "blob_foo",
					Domain:     []string{"testing"},
					Entities:   []string{"foo"},
					Confidence: 0.9,
					Sources:    1,
					Refs:       []string{},
				},
				Body:       "Foo body.",
				CommitHash: "abc123",
			},
			Score: 95.0,
		},
	}, nil)

	handler := QueryHandler(gs, idx, testAgentBranch)

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




	handler := QueryHandler(gs, idx, testAgentBranch)

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



	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return(nil, nil)

	handler := QueryHandler(gs, idx, testAgentBranch)

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



	idx.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		r := testSearchResult("general/foo.md", "Foo", "body")
		r.Domain = []string{"infra"}
		return []SearchResult{r}, nil
	})

	handler := QueryHandler(gs, idx, testAgentBranch)

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



	idx.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		r := testSearchResult("general/bar.md", "Bar", "body")
		r.Entities = []string{"db"}
		return []SearchResult{r}, nil
	})

	handler := QueryHandler(gs, idx, testAgentBranch)

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



	idx.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		return []SearchResult{testSearchResult("general/ops/deploy.md", "Deploy", "body")}, nil
	})

	handler := QueryHandler(gs, idx, testAgentBranch)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{
		"path": "general/ops",
	}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %v", result.Content)
	}

	if lastQuery.Path != "general/ops" {
		t.Fatalf("expected Path=general/ops in query, got: %q", lastQuery.Path)
	}
}

func TestQueryFrontmatterIncludesEvidenceWeight(t *testing.T) {
	// Regression: frontmatterOutput was missing evidence_weight.
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]SearchResult{
		{
			FactWithBody: FactWithBody{
				FactRecord: FactRecord{
					Path:           "general/ew.md",
					Title:          "EW Fact",
					Domain:         []string{"testing"},
					Entities:       []string{},
					Confidence:     0.8,
					Sources:        2,
					Refs:           []string{},
					EvidenceWeight: 0.6,
				},
				Body: "Body.",
			},
			Score: 90.0,
		},
	}, nil)

	handler := QueryHandler(gs, idx, testAgentBranch)
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]interface{}{"text": "ew"}

	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool error: %s", getResultText(t, result))
	}

	var resp struct {
		Facts []struct {
			Frontmatter struct {
				EvidenceWeight float64 `json:"evidence_weight"`
				Confidence     float64 `json:"confidence"`
				Sources        int     `json:"sources"`
			} `json:"frontmatter"`
		} `json:"facts"`
	}
	if err := json.Unmarshal([]byte(getResultText(t, result)), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(resp.Facts))
	}
	fm := resp.Facts[0].Frontmatter
	if fm.EvidenceWeight != 0.6 {
		t.Errorf("evidence_weight: got %v, want 0.6", fm.EvidenceWeight)
	}
	if fm.Confidence != 0.8 {
		t.Errorf("confidence: got %v, want 0.8", fm.Confidence)
	}
	if fm.Sources != 2 {
		t.Errorf("sources: got %v, want 2", fm.Sources)
	}
}

func TestQueryMinConfidenceFilter(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)
	idx := NewMockSearchIndex(ctrl)

	var lastQuery SearchQuery



	idx.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, q SearchQuery) ([]SearchResult, error) {
		lastQuery = q
		r := testSearchResult("general/sure.md", "Sure", "body")
		r.Confidence = 0.95
		return []SearchResult{r}, nil
	})

	handler := QueryHandler(gs, idx, testAgentBranch)

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
