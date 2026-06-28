package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubActivityProvider implements activityProvider for tests.
type stubActivityProvider struct {
	result store.ActivityResult
	err    error
}

func (s *stubActivityProvider) Activity(_ *repos.RepoInstance, _, _ string) (store.ActivityResult, error) {
	return s.result, s.err
}

func TestHandleHALActivity_ReturnsHAL(t *testing.T) {
	provider := &stubActivityProvider{
		result: store.ActivityResult{
			LastCommit: "2024-01-15T10:00:00Z",
			Total:      100,
			Changes7d:  5,
			Changes30d: 20,
			Changes90d: 45,
		},
	}
	s := &Server{
		Manager:          newTestManagerWithRepos(t, "alpha"),
		activityProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/activity", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		LastCommit string      `json:"last_commit"`
		Total      int         `json:"total"`
		Changes7d  int         `json:"changes_7d"`
		Changes30d int         `json:"changes_30d"`
		Changes90d int         `json:"changes_90d"`
		Links      hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Total != 100 {
		t.Errorf("total: %d, want 100", body.Total)
	}
	if body.Changes7d != 5 {
		t.Errorf("changes_7d: %d, want 5", body.Changes7d)
	}
	if body.Changes30d != 20 {
		t.Errorf("changes_30d: %d, want 20", body.Changes30d)
	}
	if body.Changes90d != 45 {
		t.Errorf("changes_90d: %d, want 45", body.Changes90d)
	}
	if body.LastCommit != "2024-01-15T10:00:00Z" {
		t.Errorf("last_commit: %q", body.LastCommit)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/activity"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALActivity_WithPath_IncludesQueryInSelf(t *testing.T) {
	provider := &stubActivityProvider{}
	s := &Server{
		Manager:          newTestManagerWithRepos(t, "alpha"),
		activityProvider: provider,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/activity?path=know/ai", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}

	var body struct {
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/activity?path=know/ai"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALActivity_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:          newTestManagerWithRepos(t),
		activityProvider: &stubActivityProvider{},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/activity", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

func TestHandleHALActivity_StoreError_Returns500(t *testing.T) {
	s := &Server{
		Manager:          newTestManagerWithRepos(t, "alpha"),
		activityProvider: &stubActivityProvider{err: errors.New("db error")},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/activity", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: %d, want 500", rec.Code)
	}
}
