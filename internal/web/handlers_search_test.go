package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubSearchProvider implements searchProvider for tests.
type stubSearchProvider struct {
	results []store.SearchResult
	err     error
	// lastQuery lets tests assert what query was built
	lastQuery store.SearchOptions
}

func (s *stubSearchProvider) Search(_ *repos.RepoInstance, _ store.Embedder, branch string, q store.SearchOptions) ([]store.SearchResult, error) {
	s.lastQuery = q
	return s.results, s.err
}

func TestHandleSearch_ReturnsHALCollection(t *testing.T) {
	provider := &stubSearchProvider{
		results: []store.SearchResult{
			{
				FactWithBody: store.FactWithBody{
					FactRecord: store.FactRecord{
						Path:       "know/ai/ml/abc123.md",
						Title:      "Attention Is All You Need",
						Type:       "observation",
						Domain:     []string{"ai", "ml"},
						Entities:   []string{"transformer"},
						Confidence: 0.9,
					},
				},
				Score: 87.5,
			},
			{
				FactWithBody: store.FactWithBody{
					FactRecord: store.FactRecord{
						Path:       "know/ai/nlp/def456.md",
						Title:      "BERT Overview",
						Type:       "claim",
						Domain:     []string{"ai"},
						Entities:   []string{"bert"},
						Confidence: 0.8,
					},
				},
				Score: 72.0,
			},
		},
	}

	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		searchProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/search?q=attention&limit=10",
		nil,
	)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Results []struct {
				Path       string      `json:"path"`
				Title      string      `json:"title"`
				Score      float64     `json:"score"`
				Type       string      `json:"type"`
				Domain     []string    `json:"domain"`
				Entities   []string    `json:"entities"`
				Confidence float64     `json:"confidence"`
				Body       *string     `json:"body,omitempty"`
				Links      hal.LinkMap `json:"_links"`
			} `json:"results"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link on collection")
	}
	if len(body.Embedded.Results) != 2 {
		t.Fatalf("embedded results: %d, want 2", len(body.Embedded.Results))
	}

	item := body.Embedded.Results[0]
	if item.Path != "know/ai/ml/abc123.md" {
		t.Errorf("path: %q", item.Path)
	}
	if item.Title != "Attention Is All You Need" {
		t.Errorf("title: %q", item.Title)
	}
	if item.Score != 87.5 {
		t.Errorf("score: %v", item.Score)
	}
	if item.Type != "observation" {
		t.Errorf("type: %q", item.Type)
	}
	if item.Body != nil {
		t.Error("body must be omitted from collection items")
	}

	// Each item must have a _links.self pointing to the fact URL.
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/facts/know/ai/ml/abc123.md"
	if got := item.Links["self"].Href; got != wantSelf {
		t.Errorf("item self link: got %q, want %q", got, wantSelf)
	}

	// Assert the query was wired correctly.
	if provider.lastQuery.Text != "attention" {
		t.Errorf("query text: %q", provider.lastQuery.Text)
	}
	if provider.lastQuery.Limit != 10 {
		t.Errorf("limit: %d, want 10", provider.lastQuery.Limit)
	}
}

func TestHandleSearch_EmptyResults(t *testing.T) {
	provider := &stubSearchProvider{results: nil}

	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		searchProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/search?q=nothing",
		nil,
	)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Count    int         `json:"count"`
		Embedded struct {
			Results []any `json:"results"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 {
		t.Errorf("count: %d, want 0", body.Count)
	}
	if len(body.Embedded.Results) != 0 {
		t.Errorf("results: %d, want 0", len(body.Embedded.Results))
	}
}

// TestHandleSearch_KindFilterReachesProvider locks in that ?kind= /
// ?exclude_kind= query params on /search reach SearchOptions.IncludeKinds /
// ExcludeKinds. Regression: the filter was silently dropped, so a UI chip
// like "kind:pragmatic" was returning every fact instead of pragmatic ones.
func TestHandleSearch_KindFilterReachesProvider(t *testing.T) {
	provider := &stubSearchProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		searchProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/search?q=x&kind=pragmatic&exclude_kind=epistemic", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := provider.lastQuery.IncludeKinds; len(got) != 1 || got[0] != "pragmatic" {
		t.Errorf("IncludeKinds: got %v, want [pragmatic]", got)
	}
	if got := provider.lastQuery.ExcludeKinds; len(got) != 1 || got[0] != "epistemic" {
		t.Errorf("ExcludeKinds: got %v, want [epistemic]", got)
	}
}

func TestHandleSearch_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:        newTestManagerWithRepos(t),
		searchProvider: &stubSearchProvider{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/search?q=x", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}
