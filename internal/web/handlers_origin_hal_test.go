package web

import (
	"context"
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
	return newControlDBTestServerOpt(t, originsRoot, true)
}

// newControlDBTestServerOpt is newControlDBTestServer with the agent key made
// optional. withKey=false leaves Deps.KeyPath pointing at a file that does not
// exist, which is how Manager.Start ends up with crypt == nil — the state in
// which Origins.Set refuses any non-empty credential rather than writing a
// secret in the clear. That refusal is the only reachable way to fail the
// DURABLE half of an origin write after its git half has already succeeded,
// which is what TestSetOrigin_FailedPersistRestoresTheGitRemote needs.
func newControlDBTestServerOpt(t *testing.T, originsRoot string, withKey bool) (*Server, *repos.Manager, *repos.RepoInstance) {
	t.Helper()
	home := t.TempDir()
	keyPath := filepath.Join(home, "agent.key")
	if withKey {
		if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
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

	// 2. The repo's own remotes table CANNOT carry connection identity —
	// migration 000017 dropped the columns, so control.db is not merely the
	// preferred source of truth, it is the only possible one. Asserting on the
	// schema rather than on a row's contents is what makes that irreversible:
	// a re-added column would fail here even before anything wrote to it.
	var cols map[string]bool
	var scanErr error
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			scanErr = errors.New("nil svc")
			return
		}
		rows, err := svc.RawDBForTest().Query(`SELECT name FROM pragma_table_info('remotes')`)
		if err != nil {
			scanErr = err
			return
		}
		defer rows.Close()
		cols = map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				scanErr = err
				return
			}
			cols[name] = true
		}
		scanErr = rows.Err()
	})
	if scanErr != nil {
		t.Fatalf("read remotes schema: %v", scanErr)
	}
	if len(cols) == 0 {
		t.Fatal("precondition: the remotes table must exist")
	}
	for _, gone := range []string{"url", "branch", "auth_method", "auth_token"} {
		if cols[gone] {
			t.Errorf("remotes.%s must not exist — connection identity belongs to control.db", gone)
		}
	}
}

// TestPatchOriginUpstream_NoOriginIsRejected pins the guard that
// SetOriginUpstream inherited from the deleted store.SetUpstreamBranch:
// changing the upstream on a repo that has no origin is an error, not a silent
// no-op that would wire up a git remote with an empty URL.
func TestPatchOriginUpstream_NoOriginIsRejected(t *testing.T) {
	s, _, _ := newControlDBTestServer(t, t.TempDir())
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/repos/alpha/origin/upstream",
		strings.NewReader(`{"branch":"develop"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code >= 200 && rec.Code < 300 {
		t.Fatalf("PATCH with no origin configured must fail; got %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no origin configured") {
		t.Errorf("error must say no origin is configured, got: %s", rec.Body.String())
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
	var refspecs []string
	ri.WithRead(func(svc *store.Service) {
		remote, err = svc.Remote().GetRemote("origin")
		refspecs = svc.FetchRefspecsForTest("origin")
	})
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil || remote.Branch != "develop" {
		t.Fatalf("live GetRemote branch: got %+v, want branch=develop", remote)
	}

	// The git FETCH REFSPEC must track the new branch. This is the assertion the
	// stored-branch checks above cannot make: drop the ConfigureRemote call from
	// SetOriginUpstream and every check above still passes while the next Sync
	// silently fetches the OLD branch. That wrong-branch sync is precisely what
	// the refspec-first ordering exists to prevent, so it has to be pinned here.
	got := make(map[string]bool, len(refspecs))
	for _, rs := range refspecs {
		got[rs] = true
	}
	if !got["+refs/heads/develop:refs/remotes/origin/develop"] {
		t.Errorf("fetch refspec must track develop after the upstream change; got %v", refspecs)
	}
	if got["+refs/heads/main:refs/remotes/origin/main"] {
		t.Errorf("the previous upstream's refspec must be gone; got %v", refspecs)
	}
	if !got["+refs/heads/"+ri.AgentBranch()+":refs/remotes/origin/"+ri.AgentBranch()] {
		t.Errorf("the agent-branch refspec must be preserved; got %v", refspecs)
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

// DeleteOrigin used to destroy the durable record BEFORE anything that could
// fail, and then discard ri.WithRead's return.
//
// ri.WithRead returns Acquire's error WITHOUT invoking the closure. Against a
// detached store — a SwapStore in flight, or a recovery reopen that failed —
// `err` therefore stayed nil, the handler wrote 204, and the URL, auth_method
// and encrypted auth_token were gone while the git remote was still configured
// and still pushing. Since this branch moved connection identity out of the
// repo database, that token exists NOWHERE else: there is nothing to recover
// from and nothing was reported.
func TestDeleteOrigin_DetachedStoreKeepsTheDurableRecord(t *testing.T) {
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
		t.Fatalf("PUT status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	// Detach the store: Acquire now fails, exactly as it does mid-swap.
	m.Remove("alpha")

	if err := (defaultOriginProvider{}).DeleteOrigin(context.Background(), m, ri); err == nil {
		t.Fatal("DeleteOrigin against a detached store returned nil; the handler answers 204 on that")
	}

	origin, err := m.Origins().Get(ri.UID())
	if err != nil {
		t.Fatalf("Origins().Get: %v", err)
	}
	if origin == nil {
		t.Fatal("the control.db record was destroyed by a delete that could not do its job; " +
			"the url, auth_method and encrypted token exist nowhere else")
	}
	if origin.URL != wantURL {
		t.Errorf("origin url: got %q, want %q", origin.URL, wantURL)
	}
	if origin.AuthMethod != "token" || origin.AuthToken != "tok-secret" {
		t.Errorf("credential must survive: got method=%q token=%q", origin.AuthMethod, origin.AuthToken)
	}
}

// SetOrigin must not record a connection it failed to wire up. The git write
// (ConfigureRemote) goes first: persisting ahead of it leaves control.db
// reporting a URL whose refspecs were never rewritten, which the next boot
// silently adopts — a PUT that answered 500 taking effect on restart. This is
// the discipline SetOriginUpstream documents; SetOrigin now keeps it too.
//
// The forced failure is a branch carrying a colon, which makes the fetch
// refspec malformed and CreateRemote refuse it.
func TestSetOrigin_FailedRefspecRewriteStoresNothing(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, ri := newControlDBTestServer(t, originsRoot)

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	err := (defaultOriginProvider{}).SetOrigin(context.Background(), m, ri, setOriginRequest{
		URL:    url,
		Branch: "bad:branch",
	})
	if err == nil {
		t.Fatal("a refspec that cannot be built must fail the call")
	}

	origin, gerr := m.Origins().Get(ri.UID())
	if gerr != nil {
		t.Fatalf("Origins().Get: %v", gerr)
	}
	if origin != nil {
		t.Fatalf("control.db recorded an origin the git config was never rewritten for: %+v", origin)
	}
}

// The other side of that ordering: ConfigureRemote succeeds and the durable
// write then fails.
//
// Nothing heals this on its own. The git remote names the NEW url while the
// injected origin — and so GetRemote, and so the reconcile loop's credential
// and upstream branch — still answer with the OLD one, so every tick fetches
// the new url carrying the old credential until someone restarts the process
// and the git config is re-derived from a record that was never written. The
// caller was told 500.
//
// The failure is not contrived: Origins.Set refuses any non-empty credential
// when the agent key could not be read, so this is what a whole class of
// installs does on every credentialed PUT.
func TestSetOrigin_FailedPersistRestoresTheGitRemote(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, ri := newControlDBTestServerOpt(t, originsRoot, false) // no agent key → crypt == nil

	first := seedBareRemoteForTest(t, filepath.Join(originsRoot, "one.git"))
	if err := (defaultOriginProvider{}).SetOrigin(context.Background(), m, ri, setOriginRequest{
		URL: first, Branch: "main", AuthMethod: "none",
	}); err != nil {
		t.Fatalf("first SetOrigin (no credential, so storable without a crypt): %v", err)
	}

	second := seedBareRemoteForTest(t, filepath.Join(originsRoot, "two.git"))
	err := (defaultOriginProvider{}).SetOrigin(context.Background(), m, ri, setOriginRequest{
		URL: second, Branch: "main", AuthMethod: "token", Token: "tok-secret",
	})
	if err == nil {
		t.Fatal("Origins.Set must refuse a credential it cannot encrypt")
	}

	origin, gerr := m.Origins().Get(ri.UID())
	if gerr != nil {
		t.Fatalf("Origins().Get: %v", gerr)
	}
	if origin == nil || origin.URL != first {
		t.Fatalf("control.db origin: got %+v, want the untouched %q", origin, first)
	}

	var urls []string
	var remote *store.Remote
	ri.WithRead(func(svc *store.Service) {
		urls = svc.RemoteURLsForTest("origin")
		remote, _ = svc.Remote().GetRemote("origin")
	})
	if len(urls) != 1 || urls[0] != first {
		t.Errorf("git remote url after a failed PUT: got %v, want [%q] — the repo is fetching a url "+
			"nothing records, with the previous origin's credential", urls, first)
	}
	if remote == nil || remote.URL != first {
		t.Errorf("GetRemote after a failed PUT: got %+v, want url=%q", remote, first)
	}
}

// SetOriginUpstream shares that ordering and therefore that window: a refspec
// rewritten to the new branch while GetRemote still reports the old one leaves
// reconcileNow reconciling against a refs/remotes/origin/<old> nothing updates
// any more. The restore has to put the refspec back.
//
// The forced failure is a live repo whose control.db row is gone — the injected
// origin still answers GetRemote, so the call gets past its "no origin
// configured" guard and into ConfigureRemote, and Origins.SetBranch then
// updates no rows and says so.
func TestSetOriginUpstream_FailedPersistRestoresTheRefspec(t *testing.T) {
	originsRoot := t.TempDir()
	_, m, ri := newControlDBTestServer(t, originsRoot)

	url := seedBareRemoteForTest(t, filepath.Join(originsRoot, "upstream.git"))
	if err := (defaultOriginProvider{}).SetOrigin(context.Background(), m, ri, setOriginRequest{
		URL: url, Branch: "main", AuthMethod: "none",
	}); err != nil {
		t.Fatalf("SetOrigin: %v", err)
	}
	if err := m.Origins().Delete(ri.UID()); err != nil {
		t.Fatalf("Origins().Delete: %v", err)
	}

	if err := (defaultOriginProvider{}).SetOriginUpstream(context.Background(), m, ri, "develop"); err == nil {
		t.Fatal("SetBranch against a missing row must fail the call")
	}

	var refspecs []string
	ri.WithRead(func(svc *store.Service) { refspecs = svc.FetchRefspecsForTest("origin") })
	got := make(map[string]bool, len(refspecs))
	for _, rs := range refspecs {
		got[rs] = true
	}
	if !got["+refs/heads/main:refs/remotes/origin/main"] {
		t.Errorf("the upstream refspec must be restored after a failed persist; got %v", refspecs)
	}
	if got["+refs/heads/develop:refs/remotes/origin/develop"] {
		t.Errorf("the refspec still tracks a branch nothing recorded; got %v", refspecs)
	}
	if !got["+refs/heads/"+ri.AgentBranch()+":refs/remotes/origin/"+ri.AgentBranch()] {
		t.Errorf("the agent-branch refspec must survive the restore; got %v", refspecs)
	}
}

// Every origin write goes through the manager's control.db tenant, which
// Manager.Close nils — and whose methods dereference their crypt on the first
// line. cmd/serve.go's 5s shutdown deadline lets a request outlive the tenant
// it is about to write to, so these must ASK for it and refuse, not dereference
// and panic into the crash-bundle path.
//
// The stopped manager here is one that was never started, which is the same
// nil-tenant state Close leaves behind. ri is nil on purpose: the guard has to
// fire before anything downstream is touched, and a provider that reached
// ri.WithRead first would panic on it.
func TestOriginWrites_RefuseAStoppedManager(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	p := defaultOriginProvider{}
	calls := []struct {
		name string
		run  func() error
	}{
		{"SetOrigin", func() error {
			return p.SetOrigin(context.Background(), m, nil, setOriginRequest{URL: "https://example.test/a.git"})
		}},
		{"SetOriginUpstream", func() error {
			return p.SetOriginUpstream(context.Background(), m, nil, "main")
		}},
		{"DeleteOrigin", func() error {
			return p.DeleteOrigin(context.Background(), m, nil)
		}},
	}
	for _, c := range calls {
		err := c.run()
		if !errors.Is(err, repos.ErrManagerStopped) {
			t.Errorf("%s against a stopped manager: got %v, want %v", c.name, err, repos.ErrManagerStopped)
		}
	}
}

// ATTACHING A REMOTE MUST NOT CHANGE THE REPO'S ONTOLOGY.
//
// "A repository is a knomit knowledge base if and only if it has an ontology"
// is enforced at CREATE — initClone refuses a remote without one, initInitialize
// refuses one that already has one. PUT /origin is the other door into exactly
// the same situation: it points an existing repo, which already has an ontology
// and facts written against it, at a remote that has its own. Nothing checked.
//
// The repo's ontology is fixed at create time and is never user-editable, so a
// remote whose taxonomy differs cannot be reconciled with it — every fact
// already written was validated against the local one, and every fact written
// afterwards would be validated against the remote's.
func TestHandleHALSetOrigin_RefusesARemoteWithADifferentOntology(t *testing.T) {
	root := t.TempDir()
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()

	// A repo on the CODE taxonomy.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"kb","mode":"preset","ontology_preset":"code"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A remote that is a knowledge base on a DIFFERENT one (the default).
	url := seedKnomitRemoteForTest(t, filepath.Join(root, "remote.git"), "seed")

	rec = httptest.NewRecorder()
	body := `{"url":"` + url + `","branch":"main"}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repos/kb/origin", strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	// The reader has to learn WHICH two taxonomies are in conflict; "conflict"
	// alone leaves them looking for something they cannot see.
	if b := rec.Body.String(); !strings.Contains(b, "source-code") || !strings.Contains(b, "general") {
		t.Fatalf("the refusal does not name both ontologies: %s", b)
	}
}

// The counterpart, and the reason this is not simply "refuse any remote with an
// ontology": attaching a remote that is NOT a knowledge base is how an existing
// local repo gets backed up. That must keep working, and the repo's own
// ontology must still be there afterwards.
func TestHandleHALSetOrigin_PlainRemoteIsAllowedAndKeepsTheOntology(t *testing.T) {
	root := t.TempDir()
	s := &Server{Manager: newRealManagerWithLocalOriginRoot(t, root)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/repos",
		strings.NewReader(`{"name":"kb","mode":"preset","ontology_preset":"code"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	url := seedPlainRemoteForTest(t, filepath.Join(root, "remote.git"))

	rec = httptest.NewRecorder()
	body := `{"url":"` + url + `","branch":"main"}`
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repos/kb/origin", strings.NewReader(body)))

	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the attach to be allowed; body = %s", rec.Code, rec.Body.String())
	}

	ri := s.Manager.Get("kb")
	if ri == nil {
		t.Fatal("repo disappeared")
	}
	if got := ri.Ontology().ID; got != "source-code" {
		t.Fatalf("ontology id = %q, want source-code — attaching a remote replaced the repo's taxonomy", got)
	}
}
