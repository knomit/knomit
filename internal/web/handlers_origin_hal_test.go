package web

import (
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

// stubOriginProvider implements originProvider for tests.
type stubOriginProvider struct {
	remote    *store.Remote
	getErr    error
	setErr    error
	deleteErr error
}

func (s *stubOriginProvider) GetOrigin(_ *repos.RepoInstance) (*store.Remote, error) {
	return s.remote, s.getErr
}

func (s *stubOriginProvider) SetOrigin(_ *repos.RepoInstance, _ setOriginRequest) error {
	return s.setErr
}

func (s *stubOriginProvider) DeleteOrigin(_ *repos.RepoInstance) error {
	return s.deleteErr
}

func TestHandleHALGetOrigin_ReturnsOriginData(t *testing.T) {
	op := &stubOriginProvider{
		remote: &store.Remote{
			Name:       "origin",
			URL:        "https://github.com/example/repo.git",
			Branch:     "main",
			AuthMethod: "token",
		},
	}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Name   string      `json:"name"`
		URL    string      `json:"url"`
		Branch string      `json:"branch"`
		Links  hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.URL != "https://github.com/example/repo.git" {
		t.Errorf("url: %q", body.URL)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if _, ok := body.Links["repo"]; !ok {
		t.Error("missing repo link")
	}
}

func TestHandleHALGetOrigin_NoOrigin_Returns204(t *testing.T) {
	op := &stubOriginProvider{remote: nil}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want 204", rec.Code)
	}
}

func TestHandleHALGetOrigin_UnknownRepo_Returns404(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

func TestHandleHALSetOrigin_Returns200(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	body := `{"url":"https://github.com/example/repo.git","auth_method":"token","token":"tok"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
}

func TestHandleHALSetOrigin_UnknownRepo_Returns404(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	body := `{"url":"https://github.com/example/repo.git"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/missing/origin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleHALSetOrigin_SetError_Returns500(t *testing.T) {
	op := &stubOriginProvider{setErr: errors.New("db error")}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	body := `{"url":"https://github.com/example/repo.git"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", rec.Code)
	}
}

func TestHandleHALDeleteOrigin_Returns204(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHALDeleteOrigin_UnknownRepo_Returns404(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/missing/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}
