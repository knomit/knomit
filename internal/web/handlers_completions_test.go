package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// stubCompletionsProvider implements completionsProvider for tests.
type stubCompletionsProvider struct {
	values []string
	err    error
}

func (s *stubCompletionsProvider) Completions(_ *repos.RepoInstance, _, _, _ string, _ int) ([]string, error) {
	return s.values, s.err
}

func TestHandleHALCompletions_ReturnsHAL(t *testing.T) {
	provider := &stubCompletionsProvider{
		values: []string{"ai", "artificial-intelligence", "alignment"},
	}
	s := &Server{
		Manager:             newTestManagerWithRepos(t, "alpha"),
		completionsProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/completions?category=domain&prefix=ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Values []string    `json:"values"`
		Links  hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Values) != 3 {
		t.Errorf("values: %d, want 3", len(body.Values))
	}
	if body.Values[0] != "ai" {
		t.Errorf("first value: %q, want ai", body.Values[0])
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	// Self link must include query params.
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/completions?category=domain&prefix=ai"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALCompletions_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:             newTestManagerWithRepos(t),
		completionsProvider: &stubCompletionsProvider{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/completions", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

func TestHandleHALCompletions_StoreError_Returns500(t *testing.T) {
	s := &Server{
		Manager:             newTestManagerWithRepos(t, "alpha"),
		completionsProvider: &stubCompletionsProvider{err: errors.New("db error")},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/completions", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}
