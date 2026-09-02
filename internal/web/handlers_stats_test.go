package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubStatsProvider implements statsProvider for tests.
type stubStatsProvider struct {
	result     store.StatsResult
	err        error
	pathPrefix string // captured from the last call so tests can assert routing.
	axis       string // captured from the last call so tests can assert axis passthrough.
}

func (s *stubStatsProvider) Stats(_ context.Context, _ *repos.RepoInstance, _, pathPrefix, axis string) (store.StatsResult, error) {
	s.pathPrefix = pathPrefix
	s.axis = axis
	return s.result, s.err
}

// The repo-scoped /stats handler never calls Highlights — only the lens union's
// axis correction does. Served from the same fixture so the two paths cannot
// disagree if a future handler starts using it.
func (s *stubStatsProvider) Highlights(_ context.Context, _ *repos.RepoInstance, _, pathPrefix, axis string) ([]store.Highlight, bool, error) {
	s.pathPrefix = pathPrefix
	s.axis = axis
	return s.result.Highlights, s.result.HighlightsFallback, s.err
}

func TestHandleHALStats_ReturnsHAL(t *testing.T) {
	provider := &stubStatsProvider{
		result: store.StatsResult{
			Total:         42,
			AvgConfidence: 0.85,
			Domains:       map[string]int{"ai": 10, "go": 5},
			Entities:      map[string]int{"transformer": 3},
		},
	}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			stats: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Total         int            `json:"total"`
		AvgConfidence float64        `json:"avg_confidence"`
		Domains       map[string]int `json:"domains"`
		Entities      map[string]int `json:"entities"`
		Links         hal.LinkMap    `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 42 {
		t.Errorf("total: %d, want 42", body.Total)
	}
	if body.AvgConfidence != 0.85 {
		t.Errorf("avg_confidence: %f", body.AvgConfidence)
	}
	if body.Domains["ai"] != 10 {
		t.Errorf("domains[ai]: %d, want 10", body.Domains["ai"])
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/stats"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALStats_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t),
		providers: storeProviders{
			stats: &stubStatsProvider{},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/stats", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

// TestHandleHALStats_ForwardsPathQuery regresses the bug where the handler
// always called provider.Stats with an empty path prefix, so domain/entity
// counts in the right panel never updated when the user navigated into a
// subdirectory.
func TestHandleHALStats_ForwardsPathQuery(t *testing.T) {
	provider := &stubStatsProvider{result: store.StatsResult{Total: 7}}
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			stats: provider,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats?path=kb/meta", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if provider.pathPrefix != "kb/meta" {
		t.Errorf("provider received pathPrefix %q, want %q", provider.pathPrefix, "kb/meta")
	}
}

func TestHandleHALStats_StoreError_Returns500(t *testing.T) {
	s := &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			stats: &stubStatsProvider{err: errors.New("db error")},
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}

func TestHandleHALStats_EmitsHighlightsTypesAndAxis(t *testing.T) {
	provider := &stubStatsProvider{
		result: store.StatsResult{
			Total:       2,
			Types:       map[string]int{"synthesis": 1, "observation": 1},
			DefaultAxis: "impact",
			Highlights: []store.Highlight{{
				Path: "kb/s/a.md", Title: "A synthesis", Type: "synthesis",
				Confidence: 0.8, Impact: 7, CommittedAt: 1780000000,
			}},
		},
	}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{stats: provider},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Types       map[string]int `json:"types"`
		DefaultAxis string         `json:"default_axis"`
		Highlights  []struct {
			Path       string  `json:"path"`
			Title      string  `json:"title"`
			Type       string  `json:"type"`
			Confidence float64 `json:"confidence"`
			Impact     int     `json:"impact"`
		} `json:"highlights"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.DefaultAxis != "impact" {
		t.Errorf("default_axis: got %q, want impact", body.DefaultAxis)
	}
	if body.Types["synthesis"] != 1 {
		t.Errorf("types[synthesis]: got %d, want 1", body.Types["synthesis"])
	}
	if len(body.Highlights) != 1 {
		t.Fatalf("highlights: got %d, want 1", len(body.Highlights))
	}
	if body.Highlights[0].Impact != 7 {
		t.Errorf("impact: got %d, want 7", body.Highlights[0].Impact)
	}
}

func TestHandleHALStats_EmptyHighlightsSerializeAsArrayNotNull(t *testing.T) {
	provider := &stubStatsProvider{result: store.StatsResult{Total: 0}}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{stats: provider},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats", nil)
	r.ServeHTTP(rec, req)

	got := rec.Body.String()
	if !strings.Contains(got, `"highlights":[]`) {
		t.Errorf("highlights must serialize as [], got: %s", got)
	}
	if !strings.Contains(got, `"types":{}`) {
		t.Errorf("types must serialize as {}, got: %s", got)
	}
}

func TestHandleHALStats_PassesAxisToProvider(t *testing.T) {
	provider := &stubStatsProvider{result: store.StatsResult{Total: 1}}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{stats: provider},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats?axis=recent", nil)
	r.ServeHTTP(rec, req)

	if provider.axis != "recent" {
		t.Errorf("axis: got %q, want recent", provider.axis)
	}
}

func TestHandleHALStats_PassesPathPrefixToProvider(t *testing.T) {
	provider := &stubStatsProvider{result: store.StatsResult{Total: 1}}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{stats: provider},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/stats?path=kb/technology/", nil)
	r.ServeHTTP(rec, req)

	if provider.pathPrefix != "kb/technology/" {
		t.Errorf("pathPrefix: got %q, want kb/technology/", provider.pathPrefix)
	}
}
