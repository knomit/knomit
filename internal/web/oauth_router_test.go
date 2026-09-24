package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth"
	"knomit/internal/store/migrate"
)

const (
	testIssuer = "https://knomit.example.com"
	alphaMCP   = "/api/v1/repos/alpha/branches/agent:test/mcp"
	initBody   = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"oauth-e2e","version":"1.0"}}}`
)

// oauthWebFixture is a Server with a real control.db (grants + OAuth
// tables), a real issuer and verifier, and a repo to reach over MCP. Its
// [auth] section is the DEFAULT: require=false and the full loopback set —
// the configuration in which re-running AuthMiddleware on the OAuth
// listener would turn a loopback caller (a same-host proxy) into anonymous
// with admin.
type oauthWebFixture struct {
	s      *Server
	store  *oauth.Store
	grants *auth.SQLGrants
}

func newOAuthWebFixture(t *testing.T, issuer string) *oauthWebFixture {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Control(db); err != nil {
		t.Fatal(err)
	}
	f := &oauthWebFixture{grants: auth.NewSQLGrants(db), store: oauth.NewStore(db, 2*time.Hour, 14*24*time.Hour)}
	f.s = &Server{
		Manager: newTestManagerWithRepos(t, "alpha"),
		Auth:    config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault},
		Grants:  f.grants,
		OAuthIssuer: oauth.NewIssuer(oauth.Options{
			Issuer: issuer, Store: f.store, Clients: oauth.NewResolver(nil), Grants: f.grants,
		}),
		BearerVerifier: oauth.NewVerifier(issuer, f.store),
	}
	return f
}

// mint issues an access token for subject with the given ceiling and
// resource, and writes the subject's grants rows.
func (f *oauthWebFixture) mint(t *testing.T, subject, resource string, ceiling []string, rows ...auth.Permission) string {
	t.Helper()
	ctx := context.Background()
	fam, err := f.store.CreateFamily(ctx, oauth.FamilySpec{ClientID: "kb", Subject: subject, Scopes: ceiling, Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.store.IssuePair(ctx, fam.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range rows {
		if err := f.grants.Grant(ctx, oauth.TokenPrincipal(subject), perm, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return out.Access
}

// withAuthorization puts the same Authorization header on every request.
func withAuthorization(h http.Handler, value string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", value)
		h.ServeHTTP(w, r)
	})
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func toolsList(t *testing.T, h http.Handler) string {
	t.Helper()
	_, sid := rpcAt(t, h, alphaMCP, "", initBody)
	out, _ := rpcAt(t, h, alphaMCP, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	raw, _ := json.Marshal(out)
	return string(raw)
}

// --- 401 ---------------------------------------------------------------------

func TestOAuthListener_NoHeaderIs401WithChallenge(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	h := f.s.OAuthHandler()
	for path, rm := range map[string]string{
		"/api/v1/repos": testIssuer + "/.well-known/oauth-protected-resource/api/v1/repos",
		alphaMCP:        testIssuer + "/.well-known/oauth-protected-resource" + alphaMCP,
	} {
		rec := serve(h, fromLoopback(httptest.NewRequest(http.MethodGet, path, nil)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401 (a loopback caller with no token is NOT anonymous here)", path, rec.Code)
		}
		ch := rec.Header().Get("WWW-Authenticate")
		if !strings.HasPrefix(ch, "Bearer ") || !strings.Contains(ch, `resource_metadata="`+rm+`"`) {
			t.Fatalf("%s: WWW-Authenticate = %q, want resource_metadata %q", path, ch, rm)
		}
		if strings.Contains(ch, "invalid_token") {
			t.Fatalf("%s: no token is not an invalid token (RFC 6750 §3.1): %q", path, ch)
		}
	}
}

// R2: any Authorization header that does not verify — any scheme, any
// shape — is 401 invalid_token with the challenge.
func TestOAuthListener_BadAuthorizationIs401InvalidToken(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	good := f.mint(t, "laptop", testIssuer, []string{"read"}, auth.Read)
	h := f.s.OAuthHandler()
	for _, v := range []string{
		"Basic dXNlcjpwYXNz",
		"Bearer",
		"Bearer ",
		"Bearer not-a-token",
		"Bearer " + good + " extra",
		"Token " + good,
		good,
	} {
		rec := serve(withAuthorization(h, v), fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)))
		ch := rec.Header().Get("WWW-Authenticate")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(ch, `error="invalid_token"`) || !strings.Contains(ch, "resource_metadata=") {
			t.Errorf("Authorization %q: %d %q; want 401 invalid_token with the challenge", v, rec.Code, ch)
		}
	}
	// Two Authorization headers, the first one valid: refused, not guessed.
	req := fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	req.Header.Add("Authorization", "Bearer "+good)
	req.Header.Add("Authorization", "Bearer garbage")
	if rec := serve(h, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("two Authorization headers: %d, want 401", rec.Code)
	}
	// The scheme is case-insensitive (RFC 7235 §2.1).
	if rec := serve(withAuthorization(h, "bearer "+good), httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)); rec.Code != http.StatusOK {
		t.Fatalf("lower-case scheme: %d %s", rec.Code, rec.Body.String())
	}
}

// --- N1: the OAuth router never runs AuthMiddleware ---------------------------

// The regression that decides the design: a LOOPBACK caller with a
// read-ceiling token, on an instance whose loopback default holds write and
// admin. If AuthMiddleware ran after the bearer middleware it would replace
// the token principal with anonymous@none and this caller would be offered
// knomit_learn. A test from httptest's default 192.0.2.1 would not notice:
// AuthMiddleware's last step keeps whatever principal is already there.
func TestOAuthListener_LoopbackTokenCallerIsTheTokenNeverAnonymous(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer, []string{"read"}, auth.Read, auth.Write)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)

	listed := toolsList(t, h) // rpcAt sends from 127.0.0.1
	if strings.Contains(listed, "knomit_learn") {
		t.Fatalf("a read-ceiling token on loopback was offered knomit_learn — it ran as anonymous: %s", listed)
	}
	if !strings.Contains(listed, "knomit_query") {
		t.Fatalf("read tools missing: %s", listed)
	}

	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/api/v1/repos/alpha/branches/agent:test/facts", strings.NewReader(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(h, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "host:laptop@token") {
		t.Fatalf("mutation by a read-ceiling token: %d %s; want 403 naming host:laptop@token", rec.Code, rec.Body.String())
	}
}

func TestOAuthListener_WriteTokenCanLearn(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer, []string{"read", "write"}, auth.Read, auth.Write)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	if listed := toolsList(t, h); !strings.Contains(listed, "knomit_learn") {
		t.Fatalf("a write token must be offered knomit_learn: %s", listed)
	}
	_, sid := rpcAt(t, h, alphaMCP, "", initBody)
	text, _ := callToolAt(t, h, alphaMCP, sid, "knomit_learn", `{}`)
	if strings.Contains(text, "permission denied") {
		t.Fatalf("a write token was refused knomit_learn by the permission gate: %s", text)
	}
}

// The ceiling narrows, it never widens: rows without write stay without it.
func TestOAuthListener_CeilingCannotWidenGrants(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer, []string{"read", "write"}, auth.Read)
	if listed := toolsList(t, withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)); strings.Contains(listed, "knomit_learn") {
		t.Fatalf("a write ceiling over read-only rows offered knomit_learn: %s", listed)
	}
}

// R6: on this listener read is REQUIRED; a token whose effective set lacks
// it is refused everywhere (403 — the token is valid, the permission is not).
func TestOAuthListener_ReadRequired(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "nobody", testIssuer, []string{"read"}) // no grants rows at all
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	for _, path := range []string{"/api/v1/repos", alphaMCP} {
		rec := serve(h, fromLoopback(httptest.NewRequest(http.MethodGet, path, nil)))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "read") {
			t.Fatalf("%s: %d %s; want 403 naming read", path, rec.Code, rec.Body.String())
		}
	}
}

// R3: a token for one MCP endpoint does not reach the rest of the API.
func TestOAuthListener_AudienceConfinesAToken(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer+alphaMCP, []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	if listed := toolsList(t, h); !strings.Contains(listed, "knomit_query") {
		t.Fatalf("the endpoint the token is for: %s", listed)
	}
	rec := serve(h, fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatalf("outside the audience: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

// A path that is not in its one spelling never reaches the audience check:
// ".." would be judged on the cleaned path while the router saw another,
// and an encoded slash is routed on RawPath by chi.
func TestOAuthListener_NonCanonicalPathRefused(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer+alphaMCP, []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	for _, target := range []string{
		alphaMCP + "/../../../../../repos",
		"/api/v1/repos/alpha/branches/agent:test%2Fmcp",
		"/api/v1/repos/alpha/branches/agent:test/mcp%2f..%2f..",
		"/api/v1//repos",
	} {
		// A deadline, because if this guard regresses the ".." request can
		// reach the MCP mount as a GET, which is an SSE stream that never
		// ends: the test must fail, not hang.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		rec := serve(h, fromLoopback(httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)))
		cancel()
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", target, rec.Code)
		}
	}
}

// --- what is public, what is absent -----------------------------------------------

// R4: the public routes answer without a token, even with require=true.
func TestOAuthListener_PublicRoutesNeedNoToken(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	f.s.Auth.Require = true
	h := f.s.OAuthHandler()
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), testIssuer+"/oauth/token") {
		t.Fatalf("AS metadata: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(h, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource"+alphaMCP, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"resource":"`+testIssuer+alphaMCP+`"`) {
		t.Fatalf("PRM path form: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader("grant_type=password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = serve(h, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_grant_type") {
		t.Fatalf("token endpoint: %d %s; want the OAuth error, not an auth refusal", rec.Code, rec.Body.String())
	}
}

// Only the API/MCP tree and the public OAuth routes live on this listener:
// /git, /docs, the SPA and the operator's approval endpoints do not.
func TestOAuthListener_OnlyTheAPITree(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	f.s.GitHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	tok := f.mint(t, "laptop", testIssuer, []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	for _, path := range []string{"/git/alpha/info/refs", "/docs", "/", "/index.html", "/api/v1/oauth/pending"} {
		if rec := serve(h, fromLoopback(httptest.NewRequest(http.MethodGet, path, nil))); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, rec.Code)
		}
	}
	if rec := serve(f.s.OAuthHandler(), fromLoopback(httptest.NewRequest(http.MethodGet, "/docs", nil))); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request to a non-public path: %d, want 401 before anything else", rec.Code)
	}
}

// --- the other listeners ignore Authorization (R2) --------------------------------------

// On the plain listener the principal comes from the kernel, the certificate
// or loopback policy, never a header: a VALID write token grants nothing
// extra to a read-only anonymous caller, and a garbage one refuses nothing.
func TestPlainListener_IgnoresAuthorization(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer, []string{"read", "write"}, auth.Read, auth.Write)
	post := func(h http.Handler) *httptest.ResponseRecorder {
		req := fromLoopback(httptest.NewRequest(http.MethodPost, "/api/v1/repos/alpha/branches/agent:test/facts", strings.NewReader(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		return serve(h, req)
	}

	f.s.Auth.LoopbackDefault = []string{"read"}
	rec := post(withAuthorization(f.s.Handler(), "Bearer "+tok))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "anonymous@none") {
		t.Fatalf("a valid token on the plain listener: %d %s; want the anonymous read-only refusal", rec.Code, rec.Body.String())
	}

	g := newOAuthWebFixture(t, testIssuer) // default loopback: write held
	rec = post(withAuthorization(g.s.Handler(), "Bearer garbage"))
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("a garbage token on the plain listener was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// D8: TokenGrants is composed where every other Grants wrapper is, in
// Server.grants(), so the MCP gate (built from it) applies the ceiling too.
func TestServerGrants_ApplyTheTokenCeiling(t *testing.T) {
	p := oauth.TokenPrincipal("laptop")
	s := &Server{Grants: auth.StaticGrants{p.String(): {auth.Read: {}, auth.Write: {}}}, Auth: config.AuthConfig{LoopbackDefault: []string{}}}
	g := s.grants()
	ctx := auth.WithCeiling(context.Background(), auth.Set{auth.Read: {}})
	if !auth.Allowed(ctx, g, p, auth.Read) || auth.Allowed(ctx, g, p, auth.Write) {
		t.Fatal("Server.grants() does not intersect a token principal's rows with its ceiling")
	}
}

// An issuer WITH a path (https://host/knomit): the proxy strips /knomit, so
// the listener sees /api/v1/...; the canonical URL is issuer + that path, and
// the challenge names the INSERTED metadata URL, which the proxy forwards
// unstripped.
func TestOAuthListener_IssuerWithPath(t *testing.T) {
	const issuer = "https://host.example/knomit"
	f := newOAuthWebFixture(t, issuer)
	h := f.s.OAuthHandler()
	rec := serve(h, fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil)))
	want := `resource_metadata="https://host.example/.well-known/oauth-protected-resource/knomit/api/v1/repos"`
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), want) {
		t.Fatalf("challenge %d %q; want %s", rec.Code, rec.Header().Get("WWW-Authenticate"), want)
	}
	root := f.mint(t, "laptop", issuer, []string{"read"}, auth.Read)
	if rec := serve(withAuthorization(h, "Bearer "+root), fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))); rec.Code != http.StatusOK {
		t.Fatalf("root token under a path issuer: %d %s", rec.Code, rec.Body.String())
	}
	// A token for the ORIGIN (not under the issuer path) is someone else's.
	other := f.mint(t, "laptop", "https://host.example", []string{"read"}, auth.Read)
	if rec := serve(withAuthorization(h, "Bearer "+other), fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token for the bare origin: %d", rec.Code)
	}
	rec = serve(h, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server/knomit", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"issuer":"`+issuer+`"`) {
		t.Fatalf("inserted AS metadata: %d %s", rec.Code, rec.Body.String())
	}
}
