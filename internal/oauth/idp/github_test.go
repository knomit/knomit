package idp_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/oauth/idp"
	"knomit/internal/oauth/idp/idptest"
)

const (
	clientID     = "Iv1.fakeclient"
	clientSecret = "fake-client-secret-bytes"
	callback     = "https://knomit.example.com/oauth/idp/callback"
	verifier     = "vvvvvvvvvvvvvvvvvvvv-._~999999999999999999999999999999"
)

func challenge() string {
	s := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func newGitHub(t *testing.T) (*idp.GitHub, *idptest.Fake) {
	t.Helper()
	f := idptest.New(t, clientID, clientSecret)
	g := idp.NewGitHub(clientID, config.NewIDPSecret(clientSecret))
	g.SetEndpoints(f.URL, f.URL) // web and API on the fake
	return g, f
}

// captureLog routes the global logger to a buffer for the test.
func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	b := &syncBuf{}
	orig := log.Logger
	log.Logger = zerolog.New(b)
	t.Cleanup(func() { log.Logger = orig })
	return b
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// signIn plays the browser at the provider: follow the authorize URL once
// and return the code the provider sends back.
func signIn(t *testing.T, g *idp.GitHub, state string) string {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(g.AuthorizeURL(state, challenge(), callback))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("state") != state {
		t.Fatalf("provider returned state %q", loc.Query().Get("state"))
	}
	return loc.Query().Get("code")
}

func TestGitHub_AuthorizeURL(t *testing.T) {
	g := idp.NewGitHub(clientID, config.NewIDPSecret(clientSecret))
	u, err := url.Parse(g.AuthorizeURL("st4te", challenge(), callback))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Fatalf("authorize endpoint %q: the host is pinned to github.com", u)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id": clientID, "redirect_uri": callback, "state": "st4te",
		"code_challenge": challenge(), "code_challenge_method": "S256",
		"allow_signup": "false",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	// No scope: /user answers the public profile, id included, with none,
	// so a leaked token is worth the least there is.
	if _, has := q["scope"]; has {
		t.Errorf("scope must be absent, got %q", q.Get("scope"))
	}
	if strings.Contains(u.String(), clientSecret) {
		t.Fatal("the authorize URL carries the client secret")
	}
}

func TestGitHub_IdentifyReturnsTheStableIDAndRevokesTheToken(t *testing.T) {
	g, f := newGitHub(t)
	logs := captureLog(t)
	code := signIn(t, g, "s1")
	sub, err := g.Identify(context.Background(), code, verifier, callback)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if sub.ID != "github-583231" || sub.Login != "octocat" {
		t.Fatalf("subject = %+v", sub)
	}
	issued := f.Issued()
	if len(issued) != 1 || len(f.Revoked()) != 1 || f.Revoked()[0] != issued[0] {
		t.Fatalf("the provider token must be revoked after the one lookup: issued %d, revoked %v", len(issued), len(f.Revoked()))
	}
	for _, u := range f.URLs() {
		for _, secret := range []string{code, verifier, clientSecret, issued[0]} {
			if strings.Contains(u, secret) && !strings.HasPrefix(u, "/login/oauth/authorize") {
				t.Errorf("request URL %q carries a secret; code, verifier and client secret go in the body", u)
			}
		}
	}
	for _, secret := range []string{code, verifier, clientSecret, issued[0]} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log carries a secret:\n%s", logs)
		}
	}
}

// Apps with expiring tokens get a refresh token without asking. It is
// discarded with the access token: never returned, never logged.
func TestGitHub_UnaskedRefreshTokenIsDiscarded(t *testing.T) {
	g, f := newGitHub(t)
	f.SetExpiringTokens(true)
	logs := captureLog(t)
	sub, err := g.Identify(context.Background(), signIn(t, g, "s"), verifier, callback)
	if err != nil || sub.ID != "github-583231" {
		t.Fatalf("Identify: %+v %v", sub, err)
	}
	for _, tok := range f.Issued() {
		if strings.Contains(logs.String(), tok) || strings.Contains(sub.Login+sub.ID, tok) {
			t.Fatalf("token %q escaped", tok)
		}
	}
}

// GitHub reports a bad code as HTTP 200 with an "error" field. That is a
// refusal, named by its code only: the description and URI are the
// provider's bytes and go nowhere.
func TestGitHub_TokenErrorAt200IsARefusal(t *testing.T) {
	g, f := newGitHub(t)
	logs := captureLog(t)
	code := signIn(t, g, "s")
	f.SetTokenError("bad_verification_code\x1b[8m")
	_, err := g.Identify(context.Background(), code, verifier, callback)
	if !errors.Is(err, idp.ErrProviderRefused) {
		t.Fatalf("err = %v, want ErrProviderRefused", err)
	}
	for _, s := range []string{err.Error(), logs.String()} {
		if strings.Contains(s, "fake says") || strings.Contains(s, "docs.example") || strings.Contains(s, "\x1b") {
			t.Fatalf("provider bytes leaked raw: %q", s)
		}
	}
	if !strings.Contains(err.Error(), `bad_verification_code\x1b[8m`) {
		t.Fatalf("the error code is named, quoted: %q", err)
	}
}

func TestGitHub_WrongSecretOrVerifierRefused(t *testing.T) {
	g, _ := newGitHub(t)
	code := signIn(t, g, "s")
	if _, err := g.Identify(context.Background(), code, verifier+"x", callback); !errors.Is(err, idp.ErrProviderRefused) {
		t.Fatalf("wrong verifier: %v", err)
	}
	bad := idp.NewGitHub(clientID, config.NewIDPSecret("wrong"))
	_, f2 := newGitHub(t)
	bad.SetEndpoints(f2.URL, f2.URL)
	if _, err := bad.Identify(context.Background(), signIn(t, bad, "s"), verifier, callback); !errors.Is(err, idp.ErrProviderRefused) {
		t.Fatalf("wrong secret: %v", err)
	}
}

// The subject is the numeric id; a /user answer without a usable one is a
// refusal, never a subject built from the login.
func TestGitHub_UserWithoutAStableIDRefused(t *testing.T) {
	for _, body := range []string{
		`{"login":"octocat"}`,
		`{"login":"octocat","id":0}`,
		`{"login":"octocat","id":-4}`,
		`{"login":"octocat","id":"583231"}`,
		`{"login":"octocat","id":1.5}`,
		`not json`,
	} {
		g, f := newGitHub(t)
		code := signIn(t, g, "s")
		f.SetUserBody(body)
		if sub, err := g.Identify(context.Background(), code, verifier, callback); !errors.Is(err, idp.ErrProviderRefused) {
			t.Errorf("/user %s: %+v %v; want ErrProviderRefused", body, sub, err)
		}
	}
}

// A body over the cap is refused, not truncated into something parseable.
func TestGitHub_OversizeUserBodyRefused(t *testing.T) {
	g, f := newGitHub(t)
	code := signIn(t, g, "s")
	f.SetUserBody(`{"login":"octocat","id":583231,"pad":"` + strings.Repeat("a", 70<<10) + `"}`)
	if _, err := g.Identify(context.Background(), code, verifier, callback); !errors.Is(err, idp.ErrProviderRefused) {
		t.Fatalf("oversize: %v", err)
	}
}

// Revocation is best-effort: a failure is a WARN without the token, and the
// identity still stands (it was proven by the lookup).
func TestGitHub_RevokeFailureIsAWarning(t *testing.T) {
	g, f := newGitHub(t)
	f.SetRevokeStatus(http.StatusInternalServerError)
	logs := captureLog(t)
	sub, err := g.Identify(context.Background(), signIn(t, g, "s"), verifier, callback)
	if err != nil || sub.ID != "github-583231" {
		t.Fatalf("Identify: %+v %v", sub, err)
	}
	if !strings.Contains(logs.String(), `"level":"warn"`) || !strings.Contains(logs.String(), "revoke") {
		t.Fatalf("no WARN for the failed revocation:\n%s", logs)
	}
	if strings.Contains(logs.String(), f.Issued()[0]) {
		t.Fatal("the WARN carries the token")
	}
}

// The provider client follows no redirect: an endpoint that answers 302
// is a failure, not a fetch from wherever it points.
func TestGitHub_NoRedirectsFollowed(t *testing.T) {
	var hit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer elsewhere.Close()
	mover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer mover.Close()
	g := idp.NewGitHub(clientID, config.NewIDPSecret(clientSecret))
	g.SetEndpoints(mover.URL, mover.URL)
	if _, err := g.Identify(context.Background(), "code", verifier, callback); !errors.Is(err, idp.ErrProviderRefused) || hit {
		t.Fatalf("redirect followed (hit=%v) or not refused: %v", hit, err)
	}
}

// W5 ruling: the provider host is pinned by the operator's choice of
// provider, so a corporate HTTPS_PROXY is honoured for it (the CIMD fetcher,
// whose URL a requester picks, keeps Proxy: nil — its own test). The proxy
// is read from the environment when the provider is built.
func TestGitHub_HonoursHTTPSProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.corp.example:3128")
	t.Setenv("NO_PROXY", "")
	g := idp.NewGitHub(clientID, config.NewIDPSecret(clientSecret))
	req, _ := http.NewRequest(http.MethodPost, "https://github.com/login/oauth/access_token", nil)
	p, err := g.ProxyFor(req)
	if err != nil || p == nil || p.Host != "proxy.corp.example:3128" {
		t.Fatalf("proxy for github.com = %v, %v; want the HTTPS_PROXY", p, err)
	}
	t.Setenv("HTTPS_PROXY", "")
	g = idp.NewGitHub(clientID, config.NewIDPSecret(clientSecret))
	if p, _ := g.ProxyFor(req); p != nil {
		t.Fatalf("no HTTPS_PROXY must mean direct, got %v", p)
	}
}

func TestGitHub_LookupLogin(t *testing.T) {
	g, _ := newGitHub(t)
	sub, err := g.LookupLogin(context.Background(), "Octocat")
	if err != nil || sub.ID != "github-583231" || sub.Login != "octocat" {
		t.Fatalf("lookup: %+v %v", sub, err)
	}
	if _, err := g.LookupLogin(context.Background(), "nobody-here"); !errors.Is(err, idp.ErrUnknownLogin) {
		t.Fatalf("unknown login: %v", err)
	}
	if _, err := g.LookupLogin(context.Background(), "../user"); err == nil {
		t.Fatal("a login outside GitHub's alphabet must be refused before any request")
	}
}
