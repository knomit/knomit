package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

func (s *stubStatsProvider) Stats(_ context.Context, _ *repos.RepoInstance, _, pathPrefix string) (store.StatsResult, error) {
	s.pathPrefix = pathPrefix
	return s.result, s.err
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
