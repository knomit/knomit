package app

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
	"knomit/internal/oauth"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// [oauth] off (the default): no OAuth router exists at all, so `knomit
// serve` opens no listener for it. On: the router is built over this
// instance's control.db and serves the issuer's documents and the 401.
func TestApp_OAuthWiredOnlyWhenConfigured(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	a, err := New(context.Background(), cfg, Options{APIOnly: true})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if a.OAuthHandler() != nil {
		a.Close()
		t.Fatal("an OAuth router exists with [oauth] unset")
	}
	a.Close()

	cfg = config.Defaults()
	cfg.Home = t.TempDir()
	cfg.OAuth.Issuer, cfg.OAuth.Addr = "http://127.0.0.1:19280", "127.0.0.1:0"
	a, err = New(context.Background(), cfg, Options{APIOnly: true})
	if err != nil {
		t.Fatalf("boot with [oauth]: %v", err)
	}
	defer a.Close()
	h := a.OAuthHandler()
	if h == nil {
		t.Fatal("no OAuth router with [oauth] set")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"issuer":"http://127.0.0.1:19280"`) {
		t.Fatalf("metadata: %d %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
	req.RemoteAddr = "127.0.0.1:1"
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("API without a token on the OAuth router: %d", rr.Code)
	}
	// The plain handler is untouched by [oauth]: loopback anonymous as ever.
	rr = httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("plain handler changed by [oauth]: %d", rr.Code)
	}
}

// W3 (F19 phase 3b): the desktop never opens the OAuth listener, so it sets
// NoOAuth and the issuer is not built even with [oauth] configured: no OAuth
// router, and the approval endpoints the web UI's pending panel probes are
// 404, which is what hides the panel.
func TestApp_NoOAuthBuildsNoIssuer(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.OAuth.Issuer, cfg.OAuth.Addr = "http://127.0.0.1:19280", "127.0.0.1:0"
	a, err := New(context.Background(), cfg, Options{APIOnly: true, NoOAuth: true})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer a.Close()
	if a.OAuthHandler() != nil {
		t.Fatal("NoOAuth built an OAuth router")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/oauth/pending", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rr := httptest.NewRecorder()
	a.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("pending with NoOAuth: %d %s; want 404", rr.Code, rr.Body.String())
	}
}

// Task 3 (F19 phase 3b): signed approval is wired to THIS instance's key
// fingerprint and to the fleet root in [tls].dir, read when a statement
// arrives — so an instance enrolled after boot can verify at once, and one
// that is not enrolled answers the named 503 rather than a 500.
func TestApp_SignedApprovalWired(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.TLS.Dir = filepath.Join(cfg.Home, "pki")
	cfg.OAuth.Issuer, cfg.OAuth.Addr = "http://127.0.0.1:19280", "127.0.0.1:0"
	a, err := New(context.Background(), cfg, Options{APIOnly: true})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer a.Close()
	h := a.OAuthHandler()
	_, pub, err := pki.LoadSigner(a.KeyPath())
	if err != nil {
		t.Fatal(err)
	}

	// Park a request as `kb login` would.
	q := url.Values{
		"response_type": {"code"}, "client_id": {"kb"}, "redirect_uri": {"http://127.0.0.1:5555/callback"},
		"code_challenge": {strings.Repeat("A", 43)}, "code_challenge_method": {"S256"},
		"state": {"s"}, "scope": {"read"}, "resource": {cfg.OAuth.Issuer},
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rr.Code, rr.Body.String())
	}
	i := strings.Index(rr.Body.String(), "/oauth/authorize/")
	id := rr.Body.String()[i+len("/oauth/authorize/") : i+len("/oauth/authorize/")+43]
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/oauth/pending/"+id, nil))
	var d oauth.Description
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil || d.Digest == "" {
		t.Fatalf("describe: %d %s", rr.Code, rr.Body.String())
	}

	fleet := pkitest.New(t)
	stmt := oauth.ApprovalStatement{Verb: oauth.VerbDeny, Instance: pki.Fingerprint(pub), ID: id, Digest: d.Digest,
		Expires: time.Now().Add(5 * time.Minute).Unix()}
	ss, err := oauth.SignStatement(stmt, fleet.Root.Signer.(ed25519.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(ss)
	post := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/oauth/approve", strings.NewReader(string(body))))
		return rr
	}

	// Not enrolled yet: no root.crt in [tls].dir.
	if rr := post(); rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "no fleet root") {
		t.Fatalf("before enrolment: %d %s; want 503 naming the missing root", rr.Code, rr.Body.String())
	}
	// Enrolled after boot: the root is read when the statement arrives.
	raw, err := os.ReadFile(filepath.Join(fleet.Dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.TLS.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.TLS.Dir, pki.RootCertFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if rr := post(); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"denied"`) {
		t.Fatalf("after enrolment: %d %s", rr.Code, rr.Body.String())
	}
}
