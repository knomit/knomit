package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/edge/oauth"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/authz"
	"knomit/internal/foundation/config"
)

// authAPIFixture is a Server carrying the P3 credential stores, the issuer
// core and an issuer set, over a real control.db. The local peer holds
// read+write in the OLD grants (as the boot seed gives the owner), because
// the old writeGate still runs in front of everything.
type authAPIFixture struct {
	s        *Server
	db       *sql.DB
	store    *oauth.Store
	bindings *authz.Store
	peer     auth.Peer
}

func newAuthAPIFixture(t *testing.T, external ...string) *authAPIFixture {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL"+store.SyncDSNParam())
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
	st := oauth.NewStoreWith(db, oauth.DefaultLifetimes(), bindings)
	cfg := config.Defaults()
	cfg.ExternalAddresses = external
	issuers, _ := cfg.Issuers()
	peer := auth.Peer{ID: "uid:501", Via: auth.LocalVia, PID: 7}
	f := &authAPIFixture{db: db, store: st, bindings: bindings, peer: peer}
	f.s = &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		Auth:      config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault},
		Grants:    auth.StaticGrants{peer.Principal().String(): {auth.Read: {}, auth.Write: {}}},
		AuthStore: st,
		AuthIssuer: oauth.NewIssuer(oauth.Options{
			Store: st, Clients: oauth.NewResolver(nil), Grants: auth.NewSQLGrants(db), WaitTimeout: 2 * time.Second,
		}),
		AuthzStore: bindings,
		Issuers:    issuers,
	}
	return f
}

func (f *authAPIFixture) local(r *http.Request) *http.Request {
	return r.WithContext(auth.WithPeer(r.Context(), f.peer))
}

func jsonReq(method, target, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// authRoutes is every /api/v1/auth route with a body that would succeed.
var authRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/api/v1/auth/links", ""},
	{http.MethodGet, "/api/v1/auth/tokens", ""},
	{http.MethodPost, "/api/v1/auth/tokens", `{"role":"reader","label":"ci","expires_in":"1h"}`},
	{http.MethodDelete, "/api/v1/auth/tokens/0123456789abcdef", ""},
	{http.MethodGet, "/api/v1/auth/sessions", ""},
	{http.MethodDelete, "/api/v1/auth/sessions/0123456789abcdef", ""},
	{http.MethodGet, "/api/v1/auth/bindings", ""},
	{http.MethodPost, "/api/v1/auth/bindings", `{"principal":"os:uid:4242","role":"reader"}`},
	{http.MethodDelete, "/api/v1/auth/bindings/os:uid:4242/reader", ""},
}

// TestAuthAPI_CreateTokenOverSocketOnly: before the flip every management
// route answers only the local authenticated listener. Plain TCP — remote,
// or the loopback anonymous principal that holds admin by default, or one
// carrying a valid PAT or session cookie — gets 403 and changes nothing.
func TestAuthAPI_CreateTokenOverSocketOnly(t *testing.T) {
	f := newAuthAPIFixture(t, "http://localhost:19278")
	h := f.s.Handler()
	cookie, _, _ := f.store.CreateSession(context.Background(), authz.Principal{Kind: authz.KindOS, ID: "uid:501"}, "")
	pat, _, _ := f.store.CreatePAT(context.Background(), authz.RoleOperator, "x", time.Now().Add(time.Hour), authz.Principal{Kind: authz.KindOS, ID: "uid:501"})
	for _, rt := range authRoutes {
		for name, mk := range map[string]func() *http.Request{
			"remote": func() *http.Request { return jsonReq(rt.method, rt.path, rt.body) },
			"anonymous loopback": func() *http.Request {
				r := fromLoopback(jsonReq(rt.method, rt.path, rt.body))
				r.Host = "localhost:19278"
				return r
			},
			"loopback with PAT and cookie": func() *http.Request {
				r := fromLoopback(jsonReq(rt.method, rt.path, rt.body))
				r.Host = "localhost:19278"
				r.Header.Set("Authorization", "Bearer "+pat)
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
				return r
			},
		} {
			rec := serve(h, mk())
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s (%s): %d %s; want 403", rt.method, rt.path, name, rec.Code, rec.Body.String())
			}
		}
	}
	if pats, _ := f.store.ListPATs(context.Background()); len(pats) != 1 {
		t.Fatalf("a refused request minted a token: %d PATs", len(pats))
	}

	// Over the socket: 201 with the secret, once.
	rec := serve(h, f.local(jsonReq(http.MethodPost, "/api/v1/auth/tokens", `{"role":"contributor","label":"laptop","expires_in":"90d"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("socket create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID, Secret, Role, Label string
		ExpiresAt               time.Time `json:"expires_at"`
	}
	decodeBody(t, rec, &out)
	if !strings.HasPrefix(out.Secret, oauth.PATPrefix) || out.Role != "contributor" || out.Label != "laptop" {
		t.Fatalf("created = %+v", out)
	}
	if d := time.Until(out.ExpiresAt); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("expires_at %v is not ~90 days out", out.ExpiresAt)
	}
	if _, err := f.store.VerifyPAT(context.Background(), out.Secret); err != nil {
		t.Fatalf("the returned secret does not verify: %v", err)
	}
	// The list never carries a secret.
	rec = serve(h, f.local(jsonReq(http.MethodGet, "/api/v1/auth/tokens", "")))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), out.Secret) || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthAPI_TokenCreateRefusals(t *testing.T) {
	f := newAuthAPIFixture(t)
	h := f.s.Handler()
	for _, body := range []string{
		`{"role":"admin","label":"x"}`,
		`{"role":"guest","label":"x"}`,
		`{"role":"owner","label":"x"}`,
		`{"role":"reader","label":"x","expires_in":"400d"}`,
		`{"role":"reader","label":"x","expires_in":"-1h"}`,
		`{"role":"reader","label":"x","expires_in":"soon"}`,
		`not json`,
		`{"role":"reader","label":"` + strings.Repeat("x", 5000) + `"}`,
	} {
		rec := serve(h, f.local(jsonReq(http.MethodPost, "/api/v1/auth/tokens", body)))
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%.60s: %d %s; want a 4xx", body, rec.Code, rec.Body.String())
		}
	}
	if pats, _ := f.store.ListPATs(context.Background()); len(pats) != 0 {
		t.Fatalf("a refused body minted a token: %+v", pats)
	}
	// Default expiry when none is given.
	rec := serve(h, f.local(jsonReq(http.MethodPost, "/api/v1/auth/tokens", `{"role":"reader","label":"d"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthAPI_RevokeTokenAndSessions(t *testing.T) {
	f := newAuthAPIFixture(t)
	h := f.s.Handler()
	ctx := context.Background()
	me := authz.Principal{Kind: authz.KindOS, ID: "uid:501"}
	secret, pat, _ := f.store.CreatePAT(ctx, authz.RoleReader, "x", time.Now().Add(time.Hour), me)
	if rec := serve(h, f.local(jsonReq(http.MethodDelete, "/api/v1/auth/tokens/"+pat.ID, ""))); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.VerifyPAT(ctx, secret); err == nil {
		t.Fatal("revoked PAT still verifies")
	}
	if rec := serve(h, f.local(jsonReq(http.MethodDelete, "/api/v1/auth/tokens/"+pat.ID, ""))); rec.Code != http.StatusNotFound {
		t.Fatalf("second revoke: %d", rec.Code)
	}
	cookie, sess, _ := f.store.CreateSession(ctx, me, "Firefox")
	rec := serve(h, f.local(jsonReq(http.MethodGet, "/api/v1/auth/sessions", "")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sess.ID) || strings.Contains(rec.Body.String(), cookie) {
		t.Fatalf("sessions: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, f.local(jsonReq(http.MethodDelete, "/api/v1/auth/sessions/"+sess.ID, ""))); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke session: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.LookupSession(ctx, cookie); err == nil {
		t.Fatal("revoked session still valid")
	}
}

func TestAuthAPI_Bindings(t *testing.T) {
	f := newAuthAPIFixture(t)
	h := f.s.Handler()
	rec := serve(h, f.local(jsonReq(http.MethodPost, "/api/v1/auth/bindings", `{"principal":"anonymous","role":"guest","label":"demo"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("bind: %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(h, f.local(jsonReq(http.MethodGet, "/api/v1/auth/bindings", "")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"principal":"anonymous"`) || !strings.Contains(rec.Body.String(), `"by":"os:uid:501"`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{
		`{"principal":"anonymous","role":"reader"}`,  // anonymous: guest only
		`{"principal":"token:abc","role":"reader"}`,  // tokens are bound at mint
		`{"principal":"node:abc","role":"node"}`,     // nodes are never bound
		`{"principal":"os:uid:007","role":"reader"}`, // not canonical
		`{"principal":"os:uid:1","role":"owner"}`,
		`{"principal":"os:uid:1","role":"bogus"}`,
	} {
		if rec := serve(h, f.local(jsonReq(http.MethodPost, "/api/v1/auth/bindings", body))); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s; want 400", body, rec.Code, rec.Body.String())
		}
	}
	if rec := serve(h, f.local(jsonReq(http.MethodDelete, "/api/v1/auth/bindings/anonymous/guest", ""))); rec.Code != http.StatusNoContent {
		t.Fatalf("unbind: %d %s", rec.Code, rec.Body.String())
	}
	if bs, _ := f.bindings.Bindings(context.Background()); len(bs) != 0 {
		t.Fatalf("still bound: %+v", bs)
	}
	if rec := serve(h, f.local(jsonReq(http.MethodDelete, "/api/v1/auth/bindings/token:abc/reader", ""))); rec.Code != http.StatusBadRequest {
		t.Fatalf("unbinding a token principal: %d; revoke the token instead", rec.Code)
	}
}

func (f *authAPIFixture) mintLink(t *testing.T, query string) string {
	t.Helper()
	rec := serve(f.s.Handler(), f.local(jsonReq(http.MethodPost, "/api/v1/auth/links"+query, "")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint link: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	decodeBody(t, rec, &out)
	if time.Until(out.ExpiresAt) > 2*time.Minute+time.Second {
		t.Fatalf("expires_at %v is more than two minutes out", out.ExpiresAt)
	}
	return out.URL
}

// follow sends GET <link> to the outer router, with Host taken from the URL.
func (f *authAPIFixture) follow(t *testing.T, link string) *httptest.ResponseRecorder {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	r.Host = u.Host
	r.Header.Set("User-Agent", "TestBrowser")
	return serve(f.s.Handler(), r)
}

func sessionSetCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

func TestLinkPage_ConsumesAndSetsCookie(t *testing.T) {
	f := newAuthAPIFixture(t, "https://kb.example", "http://localhost:19278")
	link := f.mintLink(t, "")
	if !strings.HasPrefix(link, "https://kb.example/auth/link/") {
		t.Fatalf("link %q is not on the first external address", link)
	}
	rec := f.follow(t, link)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("%d Location %q; want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionSetCookie(rec)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", c)
	}
	sess, err := f.store.LookupSession(context.Background(), c.Value)
	if err != nil || sess.Principal != (authz.Principal{Kind: authz.KindOS, ID: "uid:501"}) || sess.UserAgent != "TestBrowser" {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers = %v", rec.Header())
	}

	// ?url= names another LISTED address; an unlisted one is refused.
	if link := f.mintLink(t, "?url="+url.QueryEscape("http://localhost:19278")); !strings.HasPrefix(link, "http://localhost:19278/auth/link/") {
		t.Fatalf("?url= link = %q", link)
	}
	rec = serve(f.s.Handler(), f.local(jsonReq(http.MethodPost, "/api/v1/auth/links?url="+url.QueryEscape("https://evil.example"), "")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unlisted ?url=: %d", rec.Code)
	}
}

func TestLinkPage_SecondUseRefused(t *testing.T) {
	f := newAuthAPIFixture(t, "https://kb.example")
	link := f.mintLink(t, "")
	if rec := f.follow(t, link); rec.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", rec.Code)
	}
	rec := f.follow(t, link)
	if rec.Code != http.StatusGone || sessionSetCookie(rec) != nil || !strings.Contains(rec.Body.String(), "expired or was already used") {
		t.Fatalf("second use: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("Content-Type %q", rec.Header().Get("Content-Type"))
	}
	// A garbage token is the same page.
	if rec := f.follow(t, "https://kb.example/auth/link/garbage"); rec.Code != http.StatusGone {
		t.Fatalf("garbage: %d", rec.Code)
	}
}

// A link followed on a Host that is not a listed external address is refused
// WITHOUT spending it: no issuer is ever derived from the Host.
func TestLinkPage_UnlistedHostKeepsTheLink(t *testing.T) {
	f := newAuthAPIFixture(t, "https://kb.example")
	link := f.mintLink(t, "")
	u, _ := url.Parse(link)
	r := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	r.Host = "evil.example"
	if rec := serve(f.s.Handler(), r); rec.Code != http.StatusNotFound || sessionSetCookie(rec) != nil {
		t.Fatalf("unlisted host: %d", rec.Code)
	}
	if rec := f.follow(t, link); rec.Code != http.StatusSeeOther {
		t.Fatalf("the link was spent by the refused request: %d", rec.Code)
	}
}

func TestAuthenticate_SecureCookieFollowsIssuerScheme(t *testing.T) {
	for _, tc := range []struct {
		addr   string
		secure bool
	}{
		{"https://kb.example", true},
		{"http://localhost:19278", false},
	} {
		f := newAuthAPIFixture(t, tc.addr)
		rec := f.follow(t, f.mintLink(t, ""))
		c := sessionSetCookie(rec)
		if rec.Code != http.StatusSeeOther || c == nil || c.Secure != tc.secure {
			t.Errorf("%s: %d cookie %+v; want Secure=%v", tc.addr, rec.Code, c, tc.secure)
		}
	}
}

func TestPairPages_StartWaitApprove(t *testing.T) {
	f := newAuthAPIFixture(t, "http://localhost:19278")
	h := f.s.Handler()
	start := httptest.NewRequest(http.MethodPost, "/auth/pair", nil)
	start.Host = "localhost:19278"
	start.Header.Set("User-Agent", "PairBrowser")
	rec := serve(h, start)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ ID, Code, Wait string }
	decodeBody(t, rec, &out)
	if out.Wait != "/auth/pair/"+out.ID+"/wait" || len(out.Code) != 7 {
		t.Fatalf("start = %+v", out)
	}
	wait := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, out.Wait, nil)
		r.Host = "localhost:19278"
		return serve(h, r)
	}
	if rec := wait(); rec.Code != http.StatusAccepted || sessionSetCookie(rec) != nil {
		t.Fatalf("undecided wait: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.s.AuthIssuer.ApprovePairing(context.Background(), out.Code, authz.Principal{Kind: authz.KindOS, ID: "uid:501"}); err != nil {
		t.Fatal(err)
	}
	rec = wait()
	c := sessionSetCookie(rec)
	if rec.Code != http.StatusOK || c == nil || c.Secure {
		t.Fatalf("approved wait: %d cookie %+v", rec.Code, c)
	}
	if sess, err := f.store.LookupSession(context.Background(), c.Value); err != nil || sess.UserAgent != "PairBrowser" {
		t.Fatalf("session %+v %v", sess, err)
	}
	if rec := wait(); rec.Code != http.StatusGone || sessionSetCookie(rec) != nil {
		t.Fatalf("second collect: %d", rec.Code)
	}
	// Unknown id, unlisted Host.
	r := httptest.NewRequest(http.MethodGet, "/auth/pair/"+strings.Repeat("A", 43)+"/wait", nil)
	r.Host = "localhost:19278"
	if rec := serve(h, r); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d", rec.Code)
	}
	start = httptest.NewRequest(http.MethodPost, "/auth/pair", nil)
	start.Host = "evil.example"
	if rec := serve(h, start); rec.Code != http.StatusNotFound {
		t.Fatalf("unlisted host: %d", rec.Code)
	}
	// A body is bounded and ignored.
	start = httptest.NewRequest(http.MethodPost, "/auth/pair", strings.NewReader(strings.Repeat("x", 1<<20)))
	start.Host = "localhost:19278"
	if rec := serve(h, start); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", rec.Code)
	}
}

// TestPlainListener_IgnoresPATAndSessionCookie: before the flip a valid
// operator PAT or an owner session cookie on the main port changes nothing:
// the old edge decides as if neither were there.
func TestPlainListener_IgnoresPATAndSessionCookie(t *testing.T) {
	f := newAuthAPIFixture(t, "http://localhost:19278")
	f.s.Auth.LoopbackDefault = []string{"read"}
	h := f.s.Handler()
	ctx := context.Background()
	owner, _ := authz.Owner()
	pat, _, _ := f.store.CreatePAT(ctx, authz.RoleOperator, "x", time.Now().Add(time.Hour), owner)
	cookie, _, _ := f.store.CreateSession(ctx, owner, "")
	post := func(remoteLoopback bool, set func(*http.Request)) *httptest.ResponseRecorder {
		r := jsonReq(http.MethodPost, "/api/v1/repos/alpha/branches/agent:test/facts", `{}`)
		if remoteLoopback {
			r = fromLoopback(r)
			r.Host = "localhost:19278"
		}
		set(r)
		return serve(h, r)
	}
	for _, loop := range []bool{true, false} {
		base := post(loop, func(*http.Request) {})
		withPAT := post(loop, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+pat) })
		withCookie := post(loop, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			r.Header.Set("Origin", "http://localhost:19278")
		})
		if base.Code != http.StatusForbidden || withPAT.Code != base.Code || withCookie.Code != base.Code {
			t.Errorf("loopback=%v: base %d, PAT %d, cookie %d; want the same refusal for all", loop, base.Code, withPAT.Code, withCookie.Code)
		}
	}
}
