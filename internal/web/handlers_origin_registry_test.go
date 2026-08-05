package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// The origin HAL handlers are the only way a user changes a repo's origin after
// creation, and control.db is the only record of that origin once the repo's
// own database is gone. These tests drive the REAL provider — not the stub the
// sibling tests use — because the thing under test is precisely that the store
// mutation and the registry write happen together.
//
// They exist as wiring guards. The funnel's semantics are pinned in
// internal/repos (origin_write_test.go, origin_writethrough_test.go); what can
// rot HERE is someone routing a handler around Manager.SetOrigin/ClearOrigin —
// back to a direct store write, or to a provider built without a Manager —
// which no test that stubs the provider could ever notice.

// newRegistryBackedServer returns a Server over a real started Manager holding
// one real repo, so the default origin provider has an actual store to write to
// and the manager has an actual control.db to write through into.
func newRegistryBackedServer(t *testing.T, repo string) (*Server, *repos.Manager) {
	t.Helper()
	home := t.TempDir()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	initRepoFile(t, home, repo)
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if m.Get(repo) == nil {
		t.Fatalf("repo %q did not open", repo)
	}
	return &Server{Manager: m, AgentBranch: "machine/test"}, m
}

// newCredentialBackedServer is newRegistryBackedServer with an agent key, so
// the registry has a Crypt and can actually store a credential — without one
// SetOriginCredential refuses (never plaintext) and the PUT would fail on the
// refusal rather than on the behaviour under test.
func newCredentialBackedServer(t *testing.T, repo string) (*Server, *repos.Manager) {
	t.Helper()
	home := t.TempDir()
	keyPath := filepath.Join(home, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("fake-key-material"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:                   config.Config{Home: home},
		AgentBranch:           "machine/test",
		KeyPath:               keyPath,
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	initRepoFile(t, home, repo)
	if _, err := m.Rescan(); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if m.Get(repo) == nil {
		t.Fatalf("repo %q did not open", repo)
	}
	return &Server{Manager: m, AgentBranch: "machine/test"}, m
}

// storeAuth reads the repo store's own auth columns, which the funnel must
// leave empty on every path.
func storeAuth(t *testing.T, m *repos.Manager, repo string) (string, string) {
	t.Helper()
	var method, token string
	var readErr error
	if err := m.Get(repo).WithRead(func(svc *store.Service) {
		method, token, readErr = svc.Remote().LegacyAuth("origin")
	}); err != nil {
		t.Fatalf("WithRead(%q): %v", repo, err)
	}
	if readErr != nil {
		t.Fatalf("LegacyAuth(%q): %v", repo, readErr)
	}
	return method, token
}

// TestSetOriginKeepsTheCredentialOutOfTheStore covers PUT
// /repos/{repo}/origin, which is how a user attaches a private origin after
// creation. There must be exactly one ciphertext, in control.db: a copy left in
// the repo's own database is a second thing to rotate, a second thing to leak,
// and the one the boot migration would later mistake for unmigrated.
func TestSetOriginKeepsTheCredentialOutOfTheStore(t *testing.T) {
	s, m := newCredentialBackedServer(t, "work")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"sup3r-s3cret"}`)))
	// 502 is the ActivateSync failure against an unreachable URL; the origin is
	// persisted by then, which is what this test is about.
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	method, token, err := m.RepoRegistry().OriginCredential("work")
	if err != nil {
		t.Fatalf("OriginCredential: %v", err)
	}
	if method != "token" || token != "sup3r-s3cret" {
		t.Errorf("control.db credential = (%q, %q), want (token, sup3r-s3cret)", method, token)
	}

	if sm, st := storeAuth(t, m, "work"); sm != "" || st != "" {
		t.Errorf("the repo store kept auth (%q, %q); the credential must live only in control.db", sm, st)
	}
}

// TestGetOriginReportsAuthMethodButNeverTheToken covers GET
// /repos/{repo}/origin, whose auth_method is the UI's only signal for whether a
// repo is authenticated. The credential moved to control.db, so a handler still
// reading the store's auth columns would render a repo with a working token as
// anonymous — a user-visible regression, not a cosmetic one.
//
// The second half is the standing constraint: the METHOD is diagnostic and may
// be shown; the TOKEN must never cross the API boundary. Asserted against the
// raw body so it catches the leak regardless of which field grows it.
func TestGetOriginReportsAuthMethodButNeverTheToken(t *testing.T) {
	const secret = "sup3r-s3cret"
	s, _ := newCredentialBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"`+secret+`"}`)))
	if put.Code != http.StatusOK && put.Code != http.StatusBadGateway {
		t.Fatalf("precondition PUT: got %d, body=%s", put.Code, put.Body.String())
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/work/origin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		URL        string `json:"url"`
		AuthMethod string `json:"auth_method"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.AuthMethod != "token" {
		t.Errorf("auth_method = %q, want token — an authenticated repo must not read as anonymous", body.AuthMethod)
	}
	if body.URL != "https://example.invalid/acme/kb.git" {
		t.Errorf("url = %q", body.URL)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("the token must never reach the API; body=%s", rec.Body.String())
	}
}

// rotateRegistryCrypt re-keys the registry so whatever ciphertext it already
// holds can no longer be decrypted — the state an agent-key rotation (or a
// corrupted auth_token column) leaves behind.
func rotateRegistryCrypt(t *testing.T, m *repos.Manager) {
	t.Helper()
	crypt, err := store.NewCrypt([]byte("a-completely-different-key-than-before"))
	if err != nil {
		t.Fatalf("new crypt: %v", err)
	}
	m.RepoRegistry().SetCrypt(crypt)
	if _, _, cerr := m.RepoRegistry().OriginCredential("work"); cerr == nil {
		t.Fatal("precondition: the stored credential is still decryptable, so this proves nothing")
	}
}

// TestSetOriginReplacesAnUndecryptableCredential pins the recovery path.
//
// After a key rotation the stored ciphertext cannot be decrypted. PUT /origin
// is the ONLY API that can replace it — so if reading the old credential is a
// precondition for writing a new one, the operator is locked out and hand-
// editing SQLite is the only way back. A request that carries its own
// credential must never consult the stored one.
func TestSetOriginReplacesAnUndecryptableCredential(t *testing.T) {
	s, m := newCredentialBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"old-token"}`)))
	if _, token, _ := m.RepoRegistry().OriginCredential("work"); token != "old-token" {
		t.Fatalf("precondition: credential = %q", token)
	}

	rotateRegistryCrypt(t, m)

	// The recovery request: same repo, a fresh credential.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"replacement-token"}`)))
	// 502 is the ActivateSync failure against an unreachable URL; anything else
	// means the write itself was refused.
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("a request carrying a new credential must not be refused: got %d, body=%s",
			rec.Code, rec.Body.String())
	}

	method, token, err := m.RepoRegistry().OriginCredential("work")
	if err != nil {
		t.Fatalf("OriginCredential after replacement: %v", err)
	}
	if method != "token" || token != "replacement-token" {
		t.Errorf("credential = (%q, %q), want the replacement", method, token)
	}
}

// TestSetOriginRefusesAPartialUpdateItCannotAuthenticate is the other half of
// the same decision. A branch-only PUT genuinely depends on the stored
// credential, and it cannot be read — so the request fails, loudly, rather than
// writing an empty credential and silently deauthenticating a repo whose owner
// only meant to change its upstream. The error names the way out.
func TestSetOriginRefusesAPartialUpdateItCannotAuthenticate(t *testing.T) {
	s, m := newCredentialBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"old-token"}`)))
	if _, token, _ := m.RepoRegistry().OriginCredential("work"); token != "old-token" {
		t.Fatalf("precondition: credential = %q", token)
	}

	rotateRegistryCrypt(t, m)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"branch":"release-2"}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "send the credential with this request") {
		t.Errorf("the error must name the recovery step; body=%s", rec.Body.String())
	}

	// And it changed nothing: the branch was not written behind a credential
	// the server could not carry forward.
	if got := activeOrigin(t, m, "work").OriginBranch; got != "main" {
		t.Errorf("upstream = %q, want main — a refused update must not half-apply", got)
	}
}

// TestDeleteOriginForgetsTheCredential is the disconnect half. A revoked token
// left behind in control.db outlives the origin it belonged to, and the next
// boot would still have something to authenticate with.
func TestDeleteOriginForgetsTheCredential(t *testing.T) {
	s, m := newCredentialBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"sup3r-s3cret"}`)))
	if _, token, _ := m.RepoRegistry().OriginCredential("work"); token == "" {
		t.Fatal("precondition: no credential was recorded, so forgetting one proves nothing")
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/work/origin", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	method, token, err := m.RepoRegistry().OriginCredential("work")
	if err != nil {
		t.Fatalf("OriginCredential: %v", err)
	}
	if method != "" || token != "" {
		t.Errorf("control.db still holds (%q, %q) after a disconnect", method, token)
	}
}

// TestSetOriginPreservesTheCredentialOnAPartialUpdate pins the fallback the
// funnel moved. Re-pointing an origin without re-entering the token used to
// reuse the store's copy; the store has none any more, so the fallback has to
// read control.db — otherwise every partial update silently deauthenticates
// the repo.
func TestSetOriginPreservesTheCredentialOnAPartialUpdate(t *testing.T) {
	s, m := newCredentialBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main",`+
			`"auth_method":"token","token":"sup3r-s3cret"}`)))
	if _, token, _ := m.RepoRegistry().OriginCredential("work"); token != "sup3r-s3cret" {
		t.Fatalf("precondition: credential = %q", token)
	}

	// Same origin, different upstream branch, no credential in the body.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"branch":"release-2"}`)))
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	method, token, err := m.RepoRegistry().OriginCredential("work")
	if err != nil {
		t.Fatalf("OriginCredential: %v", err)
	}
	if method != "token" || token != "sup3r-s3cret" {
		t.Errorf("credential after a partial update = (%q, %q), want it unchanged", method, token)
	}
	if got := activeOrigin(t, m, "work").OriginBranch; got != "release-2" {
		t.Errorf("control.db upstream = %q, want release-2", got)
	}
}

func activeOrigin(t *testing.T, m *repos.Manager, repo string) repos.RepoRecord {
	t.Helper()
	rec, ok, err := m.RepoRegistry().ActiveRecord(repo)
	if err != nil {
		t.Fatalf("ActiveRecord(%q): %v", repo, err)
	}
	if !ok {
		t.Fatalf("no active registry row for %q", repo)
	}
	return rec
}

// TestSetOriginReachesTheRegistry covers PUT /repos/{repo}/origin.
//
// The 502 is the point, not an accident of the fixture. ActivateSync runs an
// immediate reconcile against the URL, which fails here because the URL is not
// a real remote — and the handler correctly reports that. The origin IS
// persisted by then, so the registry write has to happen BEFORE the activation
// that can bail out. Placing it after would skip the write-through for exactly
// the origins whose first sync failed, which are the ones most likely to still
// be sitting there unreachable when the repo later needs rebuilding.
func TestSetOriginReachesTheRegistry(t *testing.T) {
	s, m := newRegistryBackedServer(t, "work")
	r := s.NewAPIRouter()

	if got := activeOrigin(t, m, "work").OriginURL; got != "" {
		t.Fatalf("precondition: origin already recorded as %q", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"master"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := activeOrigin(t, m, "work")
	if got.OriginURL != "https://example.invalid/acme/kb.git" {
		t.Errorf("control.db origin = %q, want the URL just configured", got.OriginURL)
	}
	if got.OriginBranch != "master" {
		t.Errorf("control.db upstream = %q, want master", got.OriginBranch)
	}
}

// TestSetOriginUpstreamReachesTheRegistry covers PATCH
// /repos/{repo}/origin/upstream. The branch is half of what the registry
// records: a repo re-pinned to a release branch and later rebuilt from a row
// still naming "main" fetches a refspec the remote may not have.
func TestSetOriginUpstreamReachesTheRegistry(t *testing.T) {
	s, m := newRegistryBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main"}`)))
	if got := activeOrigin(t, m, "work").OriginBranch; got != "main" {
		t.Fatalf("precondition: upstream = %q, want main", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/repos/work/origin/upstream",
		strings.NewReader(`{"branch":"release-2"}`))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	if got := activeOrigin(t, m, "work").OriginBranch; got != "release-2" {
		t.Errorf("control.db upstream = %q, want release-2", got)
	}
}

// TestDeleteOriginClearsTheRegistry covers DELETE /repos/{repo}/origin, which
// is the direction a "never overwrite with a blank" rule silently gets wrong.
//
// A registry that kept the URL the user just disconnected would have the next
// boot re-clone this repo from a remote they deliberately detached — using
// credentials they may well have revoked in the same breath.
func TestDeleteOriginClearsTheRegistry(t *testing.T) {
	s, m := newRegistryBackedServer(t, "work")
	r := s.NewAPIRouter()

	put := httptest.NewRecorder()
	r.ServeHTTP(put, httptest.NewRequest(http.MethodPut, "/repos/work/origin",
		strings.NewReader(`{"url":"https://example.invalid/acme/kb.git","branch":"main"}`)))
	if got := activeOrigin(t, m, "work").OriginURL; got == "" {
		t.Fatal("precondition: origin was never recorded, so clearing it proves nothing")
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/work/origin", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	got := activeOrigin(t, m, "work")
	if got.OriginURL != "" {
		t.Errorf("control.db still holds origin %q after a disconnect", got.OriginURL)
	}
	if got.OriginBranch != "" {
		t.Errorf("control.db still holds upstream %q after a disconnect", got.OriginBranch)
	}
}
