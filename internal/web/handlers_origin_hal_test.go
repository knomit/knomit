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

// stubOriginProvider implements originProvider for tests.
type stubOriginProvider struct {
	remote         *store.Remote
	getErr         error
	setErr         error
	deleteErr      error
	upstreamErr    error
	upstreamBranch string // captures the branch passed to SetOriginUpstream
}

func (s *stubOriginProvider) GetOrigin(_ context.Context, _ *repos.RepoInstance) (*store.Remote, error) {
	return s.remote, s.getErr
}

func (s *stubOriginProvider) SetOrigin(_ context.Context, _ *repos.RepoInstance, _ setOriginRequest) error {
	return s.setErr
}

func (s *stubOriginProvider) SetOriginUpstream(_ context.Context, _ *repos.RepoInstance, branch string) error {
	s.upstreamBranch = branch
	return s.upstreamErr
}

func (s *stubOriginProvider) DeleteOrigin(_ context.Context, _ *repos.RepoInstance) error {
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

// TestHandleHALSetOrigin_LocalOriginGate pins that PUT /origin rejects a local
// filesystem origin when local origins are disabled (no LocalOriginRoot). The
// gate lives in the handler (which holds the real Manager), so it fires even
// though the injected provider is a bare stub — i.e. it cannot be bypassed by a
// provider constructed without an enforcement hook. Regression for the previous
// fail-open design where a nil provider field silently disabled enforcement.
func TestHandleHALSetOrigin_LocalOriginGate(t *testing.T) {
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"), // Deps{} → no LocalOriginRoot
		originProvider: &stubOriginProvider{},
	}
	r := s.NewAPIRouter()

	body := `{"url":"/etc/passwd","auth_method":"none"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
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

// TestHandleHALSetOrigin_ActivateError_Returns502 pins the contract that when
// SetOrigin persists the row successfully but ActivateSync fails (bad token,
// unreachable origin), the response is a 502 problem detail rather than the
// misleading 200 OK the previous code returned. Without this, a user retrying
// with a corrected token could not distinguish a fixed state from a broken one.
func TestHandleHALSetOrigin_ActivateError_Returns502(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha",
		StartSync: func(string) error {
			return errors.New("auth failed: bad token")
		},
	})
	m.Set("alpha", ri)

	s := &Server{Manager: m, originProvider: &stubOriginProvider{}}
	r := s.NewAPIRouter()

	body := `{"url":"https://github.com/example/repo.git","auth_method":"token","token":"tok"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
	if !strings.Contains(rec.Body.String(), "auth failed: bad token") {
		t.Errorf("body should include underlying error; got %s", rec.Body.String())
	}
}

func TestHandleHALSetOriginUpstream_UpdatesBranch(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
		strings.NewReader(`{"branch":"main"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if op.upstreamBranch != "main" {
		t.Errorf("provider got branch %q, want %q", op.upstreamBranch, "main")
	}
}

func TestHandleHALSetOriginUpstream_EmptyBranch_Returns400(t *testing.T) {
	op := &stubOriginProvider{}
	s := &Server{
		Manager:        newTestManagerWithRepos(t, "alpha"),
		originProvider: op,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
		strings.NewReader(`{"branch":""}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleHALSetOriginUpstream_InvalidBranch_Returns400 pins that a branch
// name with characters illegal in a git ref is rejected with 400 and never
// reaches the provider (it would otherwise be woven into the fetch refspec).
func TestHandleHALSetOriginUpstream_InvalidBranch_Returns400(t *testing.T) {
	for _, bad := range []string{"has space", "ends/", "-leading", "a..b", "ctrl\tname", "co:lon"} {
		op := &stubOriginProvider{}
		s := &Server{
			Manager:        newTestManagerWithRepos(t, "alpha"),
			originProvider: op,
		}
		r := s.NewAPIRouter()

		rec := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]string{"branch": bad})
		req := httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
			strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("branch %q: status got %d, want 400; body=%s", bad, rec.Code, rec.Body.String())
		}
		if op.upstreamBranch != "" {
			t.Errorf("branch %q: provider must not be called, got %q", bad, op.upstreamBranch)
		}
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
