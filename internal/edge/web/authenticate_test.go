package web

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/edge/oauth"
	"knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki/pkitest"
)

// authnFixture is the authenticator over a real control.db: seeded roles,
// the credential store, and two listed external addresses.
type authnFixture struct {
	db       *sql.DB
	store    *oauth.Store
	bindings *authz.Store
	a        *authenticator
	owner    authz.Principal
}

func newAuthnFixture(t *testing.T) *authnFixture {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Control(db); err != nil {
		t.Fatal(err)
	}
	bindings := authz.NewStore(db)
	if err := bindings.SeedRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	owner, err := authz.Owner()
	if err != nil {
		t.Fatal(err)
	}
	store := oauth.NewStoreWith(db, oauth.DefaultLifetimes(), bindings)
	cfg := config.Defaults()
	cfg.ExternalAddresses = []string{"https://kb.example", "http://localhost:19278"}
	issuers, _ := cfg.Issuers()
	return &authnFixture{
		db: db, store: store, bindings: bindings, owner: owner,
		a: &authenticator{
			mat:      authz.NewMaterializer(bindings, owner, authz.MaterializerOptions{}),
			sessions: store, pats: store, bearer: store,
			issuers: issuers,
		},
	}
}

// seen records what the next handler saw.
type seen struct {
	called bool
	policy authz.Policy
	ok     bool
}

// serve runs r through the authenticator WITHOUT the test seam: these tests
// pin authentication itself, so injectedPolicy is cleared for the call.
func (f *authnFixture) serve(t *testing.T, r *http.Request) (*httptest.ResponseRecorder, *seen) {
	t.Helper()
	saved := injectedPolicy
	injectedPolicy = nil
	defer func() { injectedPolicy = saved }()
	s := &seen{}
	w := httptest.NewRecorder()
	f.a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called = true
		s.policy, s.ok = authz.PolicyFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(w, r)
	return w, s
}

func (f *authnFixture) pat(t *testing.T, role authz.Role) (string, oauth.PAT) {
	t.Helper()
	secret, pat, err := f.store.CreatePAT(context.Background(), role, "test", time.Now().Add(time.Hour), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	return secret, pat
}

func (f *authnFixture) session(t *testing.T) string {
	t.Helper()
	cookie, _, err := f.store.CreateSession(context.Background(), f.owner, "test")
	if err != nil {
		t.Fatal(err)
	}
	return cookie
}

// oauthToken mints an OAuth access token for a family whose resource is res.
func (f *authnFixture) oauthToken(t *testing.T, res string) (string, oauth.Family) {
	t.Helper()
	ctx := context.Background()
	fam, err := f.store.CreateFamily(ctx, oauth.FamilySpec{ClientID: "kb", Subject: "laptop", Scopes: []string{"read"}, Resource: res})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := f.store.IssuePair(ctx, fam.ID)
	if err != nil {
		t.Fatal(err)
	}
	return pair.Access, fam
}

func basic(user, pw string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
}

func req(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "kb.example"
	return r
}

func TestAuthenticate_InvalidCredentialIs401NeverAnonymous(t *testing.T) {
	f := newAuthnFixture(t)
	ctx := context.Background()

	expiredSecret, _, err := f.store.CreatePAT(ctx, authz.RoleReader, "short", time.Now().Add(time.Second), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	revokedCookie := f.session(t)
	sessions, _ := f.store.ListSessions(ctx)
	if err := f.store.RevokeSession(ctx, sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // the PAT's whole-second expiry passes

	cases := map[string]func(*http.Request){
		"bad bearer":       func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
		"bad PAT bearer":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+oauth.PATPrefix+"nope") },
		"empty bearer":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"bad basic":        func(r *http.Request) { r.Header.Set("Authorization", basic("x-token", "nope")) },
		"basic not base64": func(r *http.Request) { r.Header.Set("Authorization", "Basic !!!") },
		"basic no password": func(r *http.Request) {
			r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("user")))
		},
		"unknown scheme": func(r *http.Request) { r.Header.Set("Authorization", "Digest abc") },
		"two headers": func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer a")
			r.Header.Add("Authorization", "Bearer b")
		},
		"bad cookie":      func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "nope"}) },
		"empty cookie":    func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: ""}) },
		"revoked session": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: revokedCookie}) },
		"expired PAT":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+expiredSecret) },
		"non-instance cert": func(r *http.Request) {
			fl := pkitest.New(t)
			m := fl.Enroll(t, "someone@example.com")
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{m.Cert}}
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			r := req(http.MethodGet, "/api/v1/repos")
			set(r)
			w, s := f.serve(t, r)
			if w.Code != http.StatusUnauthorized || s.called {
				t.Fatalf("status %d, next called %v; want 401 and next not called", w.Code, s.called)
			}
			if !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Bearer ") {
				t.Errorf("challenge = %q, want Bearer", w.Header().Get("WWW-Authenticate"))
			}
		})
	}

	// A dead cookie is also deleted.
	r := req(http.MethodGet, "/api/v1/repos")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: revokedCookie})
	w, _ := f.serve(t, r)
	if sc := w.Header().Get("Set-Cookie"); !strings.Contains(sc, sessionCookie+"=;") || !strings.Contains(sc, "Max-Age=0") {
		t.Errorf("Set-Cookie = %q, want the session cookie expired", sc)
	}
	// A bad token on /git gets the Basic challenge.
	r = req(http.MethodGet, "/git/alpha/info/refs?service=git-upload-pack")
	r.Header.Set("Authorization", basic("x", "nope"))
	w, _ = f.serve(t, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Basic realm="knomit"` {
		t.Errorf("git: %d %q, want 401 Basic realm=\"knomit\"", w.Code, w.Header().Get("WWW-Authenticate"))
	}
}

// TestAuthenticate_LoopbackProxyGainsNothing: a same-host proxy forwarding a
// request with no credential is anonymous, with no permission while anonymous
// holds no binding, whatever its peer address and Host.
func TestAuthenticate_LoopbackProxyGainsNothing(t *testing.T) {
	f := newAuthnFixture(t)
	for _, host := range []string{"evil.example", "localhost:19278", "kb.example", "127.0.0.1"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:5555", host
		w, s := f.serve(t, r)
		if w.Code != http.StatusNoContent || !s.called || !s.ok {
			t.Fatalf("Host %s: %d, called %v", host, w.Code, s.called)
		}
		if s.policy.Principal != authz.Anonymous() || s.policy.Via != authz.ViaNone {
			t.Errorf("Host %s: principal %v via %s, want anonymous via none", host, s.policy.Principal, s.policy.Via)
		}
		if s.policy.Owner || len(s.policy.Permissions()) != 0 || s.policy.Has(authz.KBRead) {
			t.Errorf("Host %s: anonymous holds %v", host, s.policy.Permissions())
		}
	}
}

func TestAuthenticate_CookieMutationNeedsSameOrigin(t *testing.T) {
	f := newAuthnFixture(t)
	cookie := f.session(t)
	with := func(method, origin, host string) (*httptest.ResponseRecorder, *seen) {
		r := httptest.NewRequest(method, "/api/v1/repos", nil)
		r.Host = host
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return f.serve(t, r)
	}
	for _, origin := range []string{"https://evil.example", "null", "http://kb.example", "https://kb.example:8443", ""} {
		w, s := with(http.MethodPost, origin, "kb.example")
		if w.Code != http.StatusForbidden || s.called || !strings.Contains(w.Body.String(), "Cross-origin request refused") {
			t.Errorf("POST Origin %q: %d called %v body %s; want 403 Cross-origin request refused", origin, w.Code, s.called, w.Body.String())
		}
	}
	if w, s := with(http.MethodPost, "https://kb.example", "kb.example"); w.Code != http.StatusNoContent || !s.called || !s.policy.Owner {
		t.Errorf("same-origin POST: %d called %v owner %v", w.Code, s.called, s.policy.Owner)
	}
	if w, s := with(http.MethodDelete, "http://localhost:19278", "localhost:19278"); w.Code != http.StatusNoContent || !s.called {
		t.Errorf("same-origin DELETE on the http address: %d", w.Code)
	}
	if w, s := with(http.MethodGet, "", "kb.example"); w.Code != http.StatusNoContent || !s.called || s.policy.Via != authz.ViaSession {
		t.Errorf("GET without Origin: %d called %v via %s", w.Code, s.called, s.policy.Via)
	}
	// An unlisted Host: safe methods work, mutations have no origin to match.
	if w, s := with(http.MethodGet, "", "other.example"); w.Code != http.StatusNoContent || !s.called {
		t.Errorf("GET on an unlisted host: %d", w.Code)
	}
	if w, _ := with(http.MethodPost, "https://other.example", "other.example"); w.Code != http.StatusForbidden {
		t.Errorf("POST on an unlisted host: %d, want 403", w.Code)
	}
}

func TestAuthenticate_BearerIgnoresCookie(t *testing.T) {
	f := newAuthnFixture(t)
	secret, pat := f.pat(t, authz.RoleReader)
	r := req(http.MethodPost, "/api/v1/repos")
	r.Header.Set("Authorization", "Bearer "+secret)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.session(t)})
	r.Header.Set("Origin", "https://evil.example") // would fail the cookie path
	w, s := f.serve(t, r)
	if w.Code != http.StatusNoContent || s.policy.Principal != pat.Principal() || s.policy.Via != authz.ViaBearer {
		t.Fatalf("%d principal %v via %s; want the PAT via bearer", w.Code, s.policy.Principal, s.policy.Via)
	}
	if s.policy.Owner || s.policy.Has(authz.KBWrite) || !s.policy.Has(authz.KBRead) {
		t.Errorf("reader PAT policy = %v", s.policy.Permissions())
	}
	// A bad bearer with a GOOD cookie is still 401: the cookie is never consulted.
	r = req(http.MethodGet, "/api/v1/repos")
	r.Header.Set("Authorization", "Bearer nope")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.session(t)})
	if w, s := f.serve(t, r); w.Code != http.StatusUnauthorized || s.called {
		t.Errorf("bad bearer + good cookie: %d", w.Code)
	}
}

func TestAuthenticate_BasicPATForGit(t *testing.T) {
	f := newAuthnFixture(t)
	secret, pat := f.pat(t, authz.RoleContributor)
	r := req(http.MethodGet, "/git/alpha/info/refs?service=git-upload-pack")
	r.Header.Set("Authorization", basic("x-token", secret))
	w, s := f.serve(t, r)
	if w.Code != http.StatusNoContent || s.policy.Principal != pat.Principal() || s.policy.Via != authz.ViaBasic {
		t.Fatalf("%d %v %s", w.Code, s.policy.Principal, s.policy.Via)
	}
	if !s.policy.Has(authz.KBRead) || s.policy.Has(authz.RepoPush) {
		t.Errorf("contributor PAT: %v (repo:push is node-only)", s.policy.Permissions())
	}
	// Anonymous on /git is not refused here: the gate (P4) answers it.
	if w, s := f.serve(t, req(http.MethodGet, "/git/alpha/info/refs?service=git-upload-pack")); w.Code != http.StatusNoContent || s.policy.Principal != authz.Anonymous() {
		t.Errorf("no credential on /git: %d %v", w.Code, s.policy.Principal)
	}
}

func TestAuthenticate_BasicUsernameIgnored(t *testing.T) {
	f := newAuthnFixture(t)
	secret, pat := f.pat(t, authz.RoleReader)
	for _, user := range []string{"x-token", "git", "", "anything at all"} {
		r := req(http.MethodGet, "/git/alpha/git-upload-pack")
		r.Header.Set("Authorization", basic(user, secret))
		w, s := f.serve(t, r)
		if w.Code != http.StatusNoContent || s.policy.Principal != pat.Principal() {
			t.Errorf("user %q: %d %v", user, w.Code, s.policy.Principal)
		}
	}
}

func TestAuthenticate_NodeCertGetsNodeRole(t *testing.T) {
	f := newAuthnFixture(t)
	fl := pkitest.New(t)
	m := fl.Enroll(t, "box1@agents.knomit.io")
	r := req(http.MethodPost, "/git/alpha/git-receive-pack")
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{m.Cert}}
	w, s := f.serve(t, r)
	if w.Code != http.StatusNoContent || s.policy.Via != authz.ViaMTLS {
		t.Fatalf("%d via %s", w.Code, s.policy.Via)
	}
	want := authz.Principal{Kind: authz.KindNode, ID: m.Fingerprint()}
	if s.policy.Principal != want || !s.policy.Has(authz.RepoPush) || !s.policy.Has(authz.KBRead) || s.policy.Has(authz.KBWrite) {
		t.Errorf("principal %v perms %v; want %v with the node role", s.policy.Principal, s.policy.Permissions(), want)
	}
}

func TestAuthenticate_SocketOwnerIsTotal(t *testing.T) {
	f := newAuthnFixture(t)
	r := req(http.MethodPost, "/api/v1/auth/tokens")
	r = r.WithContext(auth.WithPeer(r.Context(), auth.Peer{ID: f.owner.ID, Via: auth.LocalVia, PID: 42}))
	// A local peer wins over anything else presented.
	r.Header.Set("Authorization", "Bearer nope")
	w, s := f.serve(t, r)
	if w.Code != http.StatusNoContent || !s.policy.Owner || s.policy.Principal != f.owner {
		t.Fatalf("%d owner %v principal %v", w.Code, s.policy.Owner, s.policy.Principal)
	}
	if s.policy.Via != authz.ViaSocket && s.policy.Via != authz.ViaPipe {
		t.Errorf("via %s", s.policy.Via)
	}
	for _, p := range authz.All() {
		if !s.policy.Has(p) {
			t.Errorf("owner lacks %s", p)
		}
	}
}

func TestAuthenticate_TokenValidOnAnyHost(t *testing.T) {
	f := newAuthnFixture(t)
	secret, pat := f.pat(t, authz.RoleReader)
	access, fam := f.oauthToken(t, "https://kb.example/api/v1/repos/alpha")
	for _, host := range []string{"kb.example", "localhost:19278", "unlisted.example"} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+secret)
		if w, s := f.serve(t, r); w.Code != http.StatusNoContent || s.policy.Principal != pat.Principal() {
			t.Errorf("PAT on %s: %d %v", host, w.Code, s.policy.Principal)
		}
		r = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alpha/facts", nil)
		r.Host = host
		r.Header.Set("Authorization", "Bearer "+access)
		w, s := f.serve(t, r)
		want := authz.Principal{Kind: authz.KindToken, ID: fam.ID}
		if w.Code != http.StatusNoContent || s.policy.Principal != want {
			t.Errorf("OAuth token on %s: %d %v", host, w.Code, s.policy.Principal)
		}
	}
	// The OAuth token's reach comes from its audience path, not a URL.
	r := req(http.MethodGet, "/api/v1/repos/alpha/facts")
	r.Header.Set("Authorization", "Bearer "+access)
	var reachAlpha, reachBeta bool
	saved := injectedPolicy
	injectedPolicy = nil
	defer func() { injectedPolicy = saved }()
	f.a.middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reachAlpha = auth.ReachesRepo(r.Context(), "alpha")
		reachBeta = auth.ReachesRepo(r.Context(), "beta")
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !reachAlpha || reachBeta {
		t.Errorf("reach alpha=%v beta=%v; want alpha only", reachAlpha, reachBeta)
	}
}

// The test seam is consulted first: a policy attached by asRole is what the
// next handler sees, whatever credential the request carries.
func TestAuthenticate_InjectedPolicyFirst(t *testing.T) {
	f := newAuthnFixture(t)
	r := asRole(req(http.MethodGet, "/api/v1/repos"), authz.RoleOperator)
	r.Header.Set("Authorization", "Bearer nope")
	s := &seen{}
	w := httptest.NewRecorder()
	f.a.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.called = true
		s.policy, s.ok = authz.PolicyFrom(r.Context())
	})).ServeHTTP(w, r)
	if !s.called || !s.policy.Has(authz.RepoManage) {
		t.Fatalf("called %v policy %v", s.called, s.policy.Permissions())
	}
}
