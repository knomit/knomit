package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
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

func (s *stubOriginProvider) SetOrigin(_ context.Context, _ *repos.Manager, _ *repos.RepoInstance, _ setOriginRequest) error {
	return s.setErr
}

func (s *stubOriginProvider) SetOriginUpstream(_ context.Context, _ *repos.Manager, _ *repos.RepoInstance, branch string) error {
	s.upstreamBranch = branch
	return s.upstreamErr
}

func (s *stubOriginProvider) DeleteOrigin(_ context.Context, _ *repos.Manager, _ *repos.RepoInstance) error {
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager:   newTestManagerWithRepos(t, "alpha"), // Deps{} → no LocalOriginRoot
		providers: storeProviders{origin: &stubOriginProvider{}},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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

	s := &Server{Manager: m, providers: storeProviders{origin: &stubOriginProvider{}}}
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
			Manager: newTestManagerWithRepos(t, "alpha"),
			providers: storeProviders{
				origin: op,
			},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
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
		Manager: newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{
			origin: op,
		},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/missing/origin", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// runGitForTest runs a git command in dir, failing the test on error. Used to
// build a real bare remote that ActivateSync's synchronous reconcile can
// fetch from without touching the network.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// seedBareRemoteForTest builds a bare git repo with one commit on "main"
// under bare and returns a file:// URL pointing at it — a local stand-in for
// a real remote so PUT /origin's synchronous ActivateSync reconcile succeeds
// without any network access.
func seedBareRemoteForTest(t *testing.T, bare string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitForTest(t, work, "add", "seed.txt")
	runGitForTest(t, work, "commit", "-m", "seed")
	runGitForTest(t, work, "push", "origin", "main")
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return "file://" + bare
}

// newControlDBTestServer boots a REAL Manager — control.db opened via
// Start(), a repo registered via the real Create path — and wires the
// production (non-stub) origin provider. Unlike the stub-based tests above,
// this exercises defaultOriginProvider itself, which is what
// TestPutOrigin_PersistsToControlDB needs in order to observe the write
// actually landing in control.db (and not the repo's own remotes row).
//
// A real agent key is written so credential encryption is available —
// otherwise Origins.Set would refuse to store the test's token. originsRoot
// becomes LocalOriginRoot so a file:// origin under it clears the
// local-origin policy gate that PUT /origin enforces at the write edge.
func newControlDBTestServer(t *testing.T, originsRoot string) (*Server, *repos.Manager, *repos.RepoInstance) {
	t.Helper()
	home := t.TempDir()
	keyPath := filepath.Join(home, "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: originsRoot},
		AgentBranch:           "agent/test",
		KeyPath:               keyPath,
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}

	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "alpha", Mode: "preset"}, nil)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	s := &Server{Manager: m}
	return s, m, ri
}

// TestPutOrigin_PersistsToControlDB pins the Task 10 contract: PUT /origin
// writes connection identity (url/branch/auth) to control.db via
// mgr.Origins(), not to the repo's own remotes row. That is what makes the
// connection survive losing the repo's .db file — control.db outlives it.
func TestPutOrigin_PersistsToControlDB(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	wantURL := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	body := `{"url":"` + wantURL + `","branch":"main","auth_method":"token","token":"tok-secret"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	// 1. control.db (mgr.Origins()) holds the full record.
	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin == nil {
		t.Fatal("expected an origin in control.db, got nil")
	}
	if origin.URL != wantURL {
		t.Errorf("origin URL: got %q, want %q", origin.URL, wantURL)
	}
	if origin.Branch != "main" {
		t.Errorf("origin branch: got %q, want %q", origin.Branch, "main")
	}
	if origin.AuthToken != "tok-secret" {
		t.Errorf("origin auth token: got %q, want %q", origin.AuthToken, "tok-secret")
	}

	// 2. The repo's own remotes row carries no connection identity — control.db
	// is the source of truth now, not the per-repo store.
	var remoteURL string
	var scanErr error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			scanErr = errors.New("nil svc")
			return
		}
		scanErr = svc.RawDBForTest().QueryRow(
			`SELECT url FROM remotes WHERE name = 'origin'`).Scan(&remoteURL)
	})
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		t.Fatalf("query remotes row: %v", scanErr)
	}
	if remoteURL != "" {
		t.Errorf("repo's own remotes row must have no url, got %q", remoteURL)
	}
}

// TestPatchOriginUpstream_PersistsToControlDB exercises the real (non-stub)
// SetOriginUpstream against control.db: after PUT establishes an origin, PATCH
// .../origin/upstream must update the stored branch in mgr.Origins() (not just
// the injected in-memory copy), preserving the refspec-first ordering
// discipline carried over from the old single-function SetUpstreamBranch.
func TestPatchOriginUpstream_PersistsToControlDB(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	putBody := `{"url":"` + url + `","branch":"main","auth_method":"none"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
		strings.NewReader(`{"branch":"develop"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin == nil || origin.Branch != "develop" {
		t.Fatalf("control.db branch: got %+v, want branch=develop", origin)
	}

	// The injected origin (and thus the next GetRemote) reflects it too,
	// without a restart.
	var remote *store.Remote
	ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil || remote.Branch != "develop" {
		t.Fatalf("live GetRemote branch: got %+v, want branch=develop", remote)
	}
}

// TestDeleteOrigin_RemovesFromControlDB exercises the real (non-stub)
// DeleteOrigin: it must remove the row from control.db AND clear the
// injected origin on the running store, so a GET right after reports none
// without a restart.
func TestDeleteOrigin_RemovesFromControlDB(t *testing.T) {
	originsRoot := t.TempDir()
	s, m, ri := newControlDBTestServer(t, originsRoot)
	r := s.NewAPIRouter()

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	putBody := `{"url":"` + url + `","branch":"main","auth_method":"none"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/alpha/origin", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin != nil {
		t.Errorf("expected no origin in control.db after delete, got %+v", origin)
	}

	var remote *store.Remote
	ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote != nil {
		t.Errorf("expected the live store to report no origin after delete, got %+v", remote)
	}
}
