package oauth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth/idp"
	"knomit/internal/oauth/idp/idptest"
	"knomit/internal/store/migrate"
)

// Consent path 3 (F19 phase 3c) end to end, in-process: the issuer on an
// httptest server, GitHub as idptest's fake, and a browser with a cookie
// jar that follows NO redirect by itself, so every hop is asserted.

const (
	idpClientID     = "Iv1.knomit-test"
	idpClientSecret = "provider-client-secret-bytes"
)

type idpFixture struct {
	*issuerFixture
	fake    *idptest.Fake
	gh      *idp.GitHub
	dbPath  string
	handler *handlerSwap
	// pagePolicy is the Referrer-Policy of the last page the browser got
	// from the callback: decide sends the Origin a browser would send from
	// it (review C1).
	pagePolicy string
}

// handlerSwap lets a test rebuild the issuer (a new allow list) behind the
// same URL, as a restart with an edited config would.
type handlerSwap struct {
	mu sync.Mutex
	h  http.Handler
}

func (s *handlerSwap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h := s.h
	s.mu.Unlock()
	h.ServeHTTP(w, r)
}

func (s *handlerSwap) set(h http.Handler) { s.mu.Lock(); s.h = h; s.mu.Unlock() }

func allowedList(entries ...string) []config.AllowedSubject {
	c := config.OAuthIDPConfig{AllowedSubjects: entries}
	return c.Allowed()
}

func newIDPFixture(t *testing.T, allowed ...string) *idpFixture {
	t.Helper()
	dir := t.TempDir()
	f := &idpFixture{issuerFixture: &issuerFixture{clock: &clock{t: time.Unix(1_790_000_000, 0)}}, handler: &handlerSwap{}}
	f.dbPath = filepath.Join(dir, "control.db")
	db := openControlDBAt(t, f.dbPath)
	f.grants = auth.NewSQLGrants(db)
	f.store = NewStore(db, 2*time.Hour, 14*24*time.Hour)
	f.store.now = f.clock.now
	f.srv = httptest.NewServer(f.handler)
	t.Cleanup(f.srv.Close)
	f.fake = idptest.New(t, idpClientID, idpClientSecret)
	f.gh = idp.NewGitHub(idpClientID, config.NewIDPSecret(idpClientSecret))
	f.gh.SetEndpoints(f.fake.URL, f.fake.URL)
	f.rebuild(allowed...)
	jar, _ := cookiejar.New(nil)
	f.browser = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

// rebuild makes a fresh issuer over the same store with this allow list.
func (f *idpFixture) rebuild(allowed ...string) {
	o := config.Defaults().OAuth
	o.Clients = []config.OAuthClient{{ID: "app", Name: "App <b>&</b>", RedirectURIs: []string{"https://app.example/cb"}}}
	var opt *IDPOptions
	if allowed != nil {
		opt = &IDPOptions{Provider: f.gh, Allowed: allowedList(allowed...)}
	}
	f.iss = NewIssuer(Options{
		Issuer: f.srv.URL, Store: f.store, Clients: NewResolver(o.EffectiveClients()), Grants: f.grants,
		WaitTimeout: time.Second, IDP: opt,
	})
	f.handler.set(f.iss.Routes())
}

// openControlDBAt is openControlDB at a known path, so a test can read the
// file's bytes afterwards.
func openControlDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Control(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// do sends a request as the browser.
func (f *idpFixture) do(t *testing.T, c *http.Client, method, rawURL string, form url.Values, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, rawURL, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp, b.String()
}

// park runs /oauth/authorize in c's jar and returns the pending id.
func (f *idpFixture) park(t *testing.T, c *http.Client, scope string) string {
	t.Helper()
	_, ch := pkce()
	q := f.authorizeQuery(ch)
	q.Set("scope", scope)
	before := pendingIDs(t, f.store)
	resp, body := f.do(t, c, http.MethodGet, f.srv.URL+"/oauth/authorize?"+q.Encode(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: %d %s", resp.StatusCode, body)
	}
	return newPendingID(t, before, pendingIDs(t, f.store))
}

// signIn goes start → provider → callback and returns the callback's
// response: the consent page, or a refusal page.
func (f *idpFixture) signIn(t *testing.T, c *http.Client, id string) (*http.Response, string, string) {
	t.Helper()
	resp, body := f.do(t, c, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	resp, _ = f.do(t, c, http.MethodGet, resp.Header.Get("Location"), nil, nil) // the provider
	cb := resp.Header.Get("Location")
	if !strings.HasPrefix(cb, f.srv.URL+"/oauth/idp/callback?") {
		t.Fatalf("provider sent the browser to %q", cb)
	}
	resp, body = f.do(t, c, http.MethodGet, cb, nil, nil)
	f.pagePolicy = resp.Header.Get("Referrer-Policy")
	return resp, body, cb
}

// browserOrigin is the Origin a browser sends on a same-origin form POST
// from a page served with the given Referrer-Policy. Fetch's "append a
// request Origin header": for a POST that is not CORS, policy no-referrer
// serializes the origin as "null" — even to the page's own origin (review
// C1, reproduced in Chromium). The other policies keep the real origin for
// a same-origin, same-scheme request.
func browserOrigin(policy, origin string) string {
	if strings.EqualFold(strings.TrimSpace(policy), "no-referrer") {
		return "null"
	}
	return origin
}

var tokenInputRE = regexp.MustCompile(`name="token" value="([^"]+)"`)

func confirmToken(t *testing.T, page string) string {
	t.Helper()
	m := tokenInputRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no confirmation token on the page:\n%s", page)
	}
	return m[1]
}

func (f *idpFixture) origin() string { return f.srv.URL }

// decide POSTs the consent form as a same-origin browser would.
func (f *idpFixture) decide(t *testing.T, c *http.Client, token, decision string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	if hdr == nil {
		hdr = map[string]string{"Origin": browserOrigin(f.pagePolicy, f.origin())}
	}
	return f.do(t, c, http.MethodPost, f.srv.URL+"/oauth/idp/decide", url.Values{"token": {token}, "decision": {decision}}, hdr)
}

func (f *idpFixture) pending(t *testing.T, id string) Pending {
	t.Helper()
	p, err := f.store.GetPending(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- the whole flow --------------------------------------------------------

// Authorize → start → provider → callback → consent page → explicit POST →
// /wait → the client's redirect with code, state and iss → token as
// host:github-583231@token.
func TestIDP_FullFlowApprovesOnlyOnTheExplicitPOST(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	ver, _ := pkce()
	id := f.park(t, f.browser, "read write")

	resp, page, _ := f.signIn(t, f.browser, id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback: %d %s", resp.StatusCode, page)
	}
	if p := f.pending(t, id); p.Decision != "" {
		t.Fatalf("the callback decided the request (%q); only the POST may", p.Decision)
	}

	resp, _ = f.decide(t, f.browser, confirmToken(t, page), "approve", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != f.srv.URL+"/oauth/authorize/"+id+"/wait" {
		t.Fatalf("decide: %d → %q; want 303 to /wait (R2)", resp.StatusCode, resp.Header.Get("Location"))
	}
	p := f.pending(t, id)
	if p.Decision != DecisionApproved || p.Subject != "github-583231" || p.DecidedBy != "idp:github:583231" ||
		strings.Join(p.Ceiling, " ") != "read write" {
		t.Fatalf("pending after approval: %+v", p)
	}

	resp, _ = f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Host != "127.0.0.1:54321" || loc.Query().Get("code") == "" ||
		loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != f.srv.URL {
		t.Fatalf("wait: %d %q", resp.StatusCode, loc)
	}
	status, doc := f.exchange(t, loc.Query().Get("code"), ver)
	if status != http.StatusOK || doc["scope"] != "read write" {
		t.Fatalf("exchange: %d %v", status, doc)
	}
	set, _ := f.grants.For(context.Background(), TokenPrincipal("github-583231"))
	if !set.Has(auth.Read) || !set.Has(auth.Write) {
		t.Fatalf("grants for the subject: %v", set)
	}
	rows, _ := f.grants.List(context.Background(), TokenPrincipal("github-583231").String())
	if len(rows) == 0 || rows[0].GrantedBy != "idp:github:583231" {
		t.Fatalf("grants must name the approving identity: %+v", rows)
	}
}

// R1 (B1): the one-click chain. The victim is sent a start link; GitHub
// skips its screen; the callback runs. Nothing is decided, and /wait
// answers "still waiting" — no code — until the human POSTs.
func TestIDP_OneClickChainYieldsNoCode(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read write")
	if resp, page, _ := f.signIn(t, f.browser, id); resp.StatusCode != http.StatusOK || !strings.Contains(page, "<button") {
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize/"+id+"/wait", nil, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Location") != "" {
		t.Fatalf("wait after the one-click chain: %d %q; want the waiting page", resp.StatusCode, resp.Header.Get("Location"))
	}
	if p := f.pending(t, id); p.Decision != "" {
		t.Fatalf("decided without the POST: %+v", p)
	}
}

// W1: a forwarded start link fails in any browser that did not park the
// request — BEFORE the provider is contacted.
func TestIDP_StartNeedsTheParkingBrowsersCookie(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read")
	jar, _ := cookiejar.New(nil)
	victim := &http.Client{Jar: jar, CheckRedirect: f.browser.CheckRedirect}
	resp, body := f.do(t, victim, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	if resp.StatusCode != http.StatusForbidden || f.fake.Authorizations() != 0 || f.iss.idp.live() != 0 {
		t.Fatalf("start from another browser: %d, provider reached %d times, %d states:\n%s",
			resp.StatusCode, f.fake.Authorizations(), f.iss.idp.live(), body)
	}
	// A cookie with the right name and the wrong value is no better.
	u, _ := url.Parse(f.srv.URL + "/oauth/idp/")
	jar.SetCookies(u, []*http.Cookie{{Name: bindingCookieName(id), Value: "forged", Path: "/oauth/idp/"}})
	if resp, _ := f.do(t, victim, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged cookie: %d", resp.StatusCode)
	}
}

// S5: start retains nothing for a stranger's id.
func TestIDP_StartRetainsNothingForUnknownIDs(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	for _, id := range []string{"nope", strings.Repeat("A", 43), strings.Repeat("x", 5000)} {
		resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("start %q…: %d", id[:4], resp.StatusCode)
		}
	}
	if f.iss.idp.live() != 0 || f.fake.Authorizations() != 0 {
		t.Fatalf("state kept for unknown ids: %d", f.iss.idp.live())
	}
	// One live state per request: a second start replaces the first.
	id := f.park(t, f.browser, "read")
	for i := 0; i < 3; i++ {
		f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	}
	if f.iss.idp.live() != 1 {
		t.Fatalf("states after three starts for one request: %d", f.iss.idp.live())
	}
}

// The consent page is the ONLY defence when a victim opens an attacker's
// authorize URL themselves: it leads with where the code goes and for which
// client, in plain words, before any button; nothing submits or focuses
// by itself; and requester text is escaped.
func TestIDP_ConsentPageLeadsWithWhereTheCodeGoes(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	_, ch := pkce()
	q := f.authorizeQuery(ch)
	q.Set("client_id", "app")
	q.Set("redirect_uri", "https://app.example/cb")
	before := pendingIDs(t, f.store)
	f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize?"+q.Encode(), nil, nil)
	id := newPendingID(t, before, pendingIDs(t, f.store))
	_, page, _ := f.signIn(t, f.browser, id)

	host, button := strings.Index(page, "app.example"), strings.Index(page, "<button")
	if host < 0 || button < 0 || host > button {
		t.Fatalf("the redirect host must come before any button (host at %d, button at %d):\n%s", host, button, page)
	}
	if !strings.Contains(page, "will send a login code to") || strings.Index(page, `for client <code>app</code>`) > button {
		t.Fatalf("the page must say in plain words where the code goes and for which client:\n%s", page)
	}
	for _, bad := range []string{"autofocus", "<script", "onload", "App <b>"} {
		if strings.Contains(page, bad) {
			t.Fatalf("consent page contains %q:\n%s", bad, page)
		}
	}
	if !strings.Contains(page, "App &lt;b&gt;&amp;&lt;/b&gt;") || !strings.Contains(page, "octocat") || !strings.Contains(page, "github-583231") {
		t.Fatalf("page must show the escaped client name and the identity:\n%s", page)
	}
}

// R1: the POST needs the binding cookie, the issuer's Origin, and a live,
// unused confirmation token. Each refusal leaves the request undecided.
func TestIDP_DecideRefusals(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	for _, tc := range []struct {
		name string
		run  func(f *idpFixture, token string) *http.Response
	}{
		{"no cookie", func(f *idpFixture, token string) *http.Response {
			r, _ := f.decide(t, &http.Client{Jar: jar, CheckRedirect: f.browser.CheckRedirect}, token, "approve", nil)
			return r
		}},
		{"foreign Origin", func(f *idpFixture, token string) *http.Response {
			r, _ := f.decide(t, f.browser, token, "approve", map[string]string{"Origin": "https://evil.example"})
			return r
		}},
		{"null Origin", func(f *idpFixture, token string) *http.Response {
			r, _ := f.decide(t, f.browser, token, "approve", map[string]string{"Origin": "null"})
			return r
		}},
		{"no Origin", func(f *idpFixture, token string) *http.Response {
			r, _ := f.decide(t, f.browser, token, "approve", map[string]string{})
			return r
		}},
		{"cross-site fetch metadata", func(f *idpFixture, token string) *http.Response {
			r, _ := f.decide(t, f.browser, token, "approve", map[string]string{"Origin": f.origin(), "Sec-Fetch-Site": "cross-site"})
			return r
		}},
		{"unknown token", func(f *idpFixture, token string) *http.Response {
			r, _ := f.decide(t, f.browser, token+"x", "approve", nil)
			return r
		}},
		{"expired token", func(f *idpFixture, token string) *http.Response {
			f.clock.add(confirmTTL + time.Second)
			r, _ := f.decide(t, f.browser, token, "approve", nil)
			return r
		}},
		{"GET", func(f *idpFixture, token string) *http.Response {
			r, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/decide?token="+token+"&decision=approve", nil, nil)
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIDPFixture(t, "github-583231")
			id := f.park(t, f.browser, "read")
			_, page, _ := f.signIn(t, f.browser, id)
			resp := tc.run(f, confirmToken(t, page))
			if resp.StatusCode < 400 {
				t.Fatalf("refused POST answered %d", resp.StatusCode)
			}
			if p := f.pending(t, id); p.Decision != "" {
				t.Fatalf("decided anyway: %+v", p)
			}
		})
	}
}

func TestIDP_ConfirmationTokenIsSingleUse(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read")
	_, page, _ := f.signIn(t, f.browser, id)
	tok := confirmToken(t, page)
	if resp, _ := f.decide(t, f.browser, tok, "deny", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first decide: %d", resp.StatusCode)
	}
	// 400 is the TOKEN refusing; 409 would be the row refusing a second
	// decision, which is the belt and not the thing under test.
	if resp, _ := f.decide(t, f.browser, tok, "approve", nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reused token: %d, want 400 from the spent token", resp.StatusCode)
	}
	if p := f.pending(t, id); p.Decision != DecisionDenied || p.DecidedBy != "idp:github:583231" {
		t.Fatalf("pending: %+v", p)
	}
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize/"+id+"/wait", nil, nil)
	if loc, _ := url.Parse(resp.Header.Get("Location")); loc.Query().Get("error") != "access_denied" {
		t.Fatalf("deny must complete the client's redirect with access_denied: %q", loc)
	}
}

// A real identity that is not on the list: the request is refused (the
// browser that parked it is the only one that can get here, so this is
// self-denial, S12), the client gets access_denied, and the operator's log
// names the id and login so they can add them.
func TestIDP_NotAllowedIdentityDenies(t *testing.T) {
	f := newIDPFixture(t, "github-1")
	logs := captureLog(t)
	id := f.park(t, f.browser, "read")
	resp, page, _ := f.signIn(t, f.browser, id)
	if resp.StatusCode != http.StatusForbidden || strings.Contains(page, "<button") || !strings.Contains(page, "not allowed") {
		t.Fatalf("callback for a stranger: %d\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "/oauth/authorize/"+id+"/wait") {
		t.Fatal("the refusal page must continue to /wait so the client hears access_denied")
	}
	if p := f.pending(t, id); p.Decision != DecisionDenied || p.DecidedBy != "idp:github:583231" {
		t.Fatalf("pending: %+v", p)
	}
	if !strings.Contains(logs.String(), "github-583231") || !strings.Contains(logs.String(), "octocat") || !strings.Contains(logs.String(), `"level":"info"`) {
		t.Fatalf("INFO log must name the refused identity:\n%s", logs)
	}
}

// Per-subject caps (S7): the entry's scopes cap the default ceiling; an
// empty intersection is a named refusal, not a 400 from ceilingFor.
func TestIDP_PerSubjectCeiling(t *testing.T) {
	f := newIDPFixture(t, "github-583231 read")
	id := f.park(t, f.browser, "read write")
	_, page, _ := f.signIn(t, f.browser, id)
	f.decide(t, f.browser, confirmToken(t, page), "approve", nil)
	if p := f.pending(t, id); strings.Join(p.Ceiling, " ") != "read" {
		t.Fatalf("capped ceiling = %v", p.Ceiling)
	}

	f2 := newIDPFixture(t, "github-583231 push:own")
	id = f2.park(t, f2.browser, "read write")
	resp, page, _ := f2.signIn(t, f2.browser, id)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(page, "not permitted for this identity") {
		t.Fatalf("empty intersection: %d\n%s", resp.StatusCode, page)
	}
	if p := f2.pending(t, id); p.Decision != DecisionDenied {
		t.Fatalf("pending: %+v", p)
	}
}

// State (S5, S6): single use, bound to the browser's cookie, and a callback
// carrying `iss` is a misrouted knomit response, refused.
func TestIDP_CallbackStateRules(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read")
	_, _, cb := f.signIn(t, f.browser, id)
	if resp, _ := f.do(t, f.browser, http.MethodGet, cb, nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", resp.StatusCode)
	}

	// Login CSRF: a provider callback started in one browser and opened in
	// another (which lacks the request's cookie) is refused.
	id = f.park(t, f.browser, "read")
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	resp, _ = f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	jar, _ := cookiejar.New(nil)
	other := &http.Client{Jar: jar, CheckRedirect: f.browser.CheckRedirect}
	if r, _ := f.do(t, other, http.MethodGet, resp.Header.Get("Location"), nil, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback in a browser without the cookie: %d", r.StatusCode)
	}

	id = f.park(t, f.browser, "read")
	resp, _ = f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	resp, _ = f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	if r, _ := f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location")+"&iss="+url.QueryEscape(f.srv.URL), nil, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback with iss: %d", r.StatusCode)
	}

	id = f.park(t, f.browser, "read")
	resp, _ = f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	resp, _ = f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	f.clock.add(stateTTL + time.Second)
	if r, _ := f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired state: %d", r.StatusCode)
	}
}

// Cancel at the provider: named, nothing decided, the page leads back.
func TestIDP_ProviderCancelIsNamedAndDecidesNothing(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	f.fake.SetDeny(true)
	id := f.park(t, f.browser, "read")
	resp, page, _ := f.signIn(t, f.browser, id)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "cancelled") || strings.Contains(page, "has denied") {
		t.Fatalf("cancel: %d\n%s", resp.StatusCode, page)
	}
	if p := f.pending(t, id); p.Decision != "" {
		t.Fatalf("pending: %+v", p)
	}
}

// A provider failure is named without its words.
func TestIDP_ProviderRefusalIsNamedWithoutItsWords(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read")
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	resp, _ = f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	f.fake.SetTokenError("bad_verification_code")
	r, page := f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	if r.StatusCode != http.StatusBadGateway || !strings.Contains(page, "GitHub") || strings.Contains(page, "fake says") || strings.Contains(page, "bad_verification_code") {
		t.Fatalf("provider refusal: %d\n%s", r.StatusCode, page)
	}
	if p := f.pending(t, id); p.Decision != "" {
		t.Fatalf("pending: %+v", p)
	}
}

// Claude Code with two knomit servers parks two requests in one browser:
// one cookie per request, so neither overwrites the other.
func TestIDP_TwoParkedRequestsInOneBrowserBothComplete(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	a := f.park(t, f.browser, "read")
	b := f.park(t, f.browser, "read")
	for _, id := range []string{b, a} {
		_, page, _ := f.signIn(t, f.browser, id)
		if resp, _ := f.decide(t, f.browser, confirmToken(t, page), "approve", nil); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("decide %s: %d", id[:6], resp.StatusCode)
		}
		resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize/"+id+"/wait", nil, nil)
		if loc, _ := url.Parse(resp.Header.Get("Location")); loc.Query().Get("code") == "" {
			t.Fatalf("request %s got no code: %q", id[:6], loc)
		}
	}
}

// The binding cookie: one per request, HttpOnly, SameSite=Lax (the provider
// callback is a cross-site top-level GET and must carry it), scoped to the
// path the BROWSER sees under the issuer, Secure on https, 10 minutes.
func TestIDP_BindingCookieAttributes(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	_, ch := pkce()
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize?"+f.authorizeQuery(ch).Encode(), nil, nil)
	cs := resp.Cookies()
	if len(cs) != 1 {
		t.Fatalf("cookies: %+v", cs)
	}
	c := cs[0]
	id := newestPending(t, f.store)
	if c.Name != bindingCookieName(id) || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/oauth/idp/" ||
		c.Secure || c.MaxAge != int(pendingTTL/time.Second) {
		t.Fatalf("cookie %+v", c)
	}
	if p := f.pending(t, id); p.IDPBinding != hashSecret(c.Value) {
		t.Fatal("the row must keep the cookie's hash")
	}

	// A path issuer behind a proxy that strips the prefix: the browser sees
	// /knomit/oauth/idp/, so that is the cookie's Path. Secure on https.
	iss := NewIssuer(Options{Issuer: "https://h.example/knomit", Store: f.store,
		Clients: NewResolver(config.Defaults().OAuth.EffectiveClients()), Grants: f.grants,
		IDP: &IDPOptions{Provider: f.gh, Allowed: allowedList("github-1")}})
	q := f.authorizeQuery(ch)
	q.Set("resource", "https://h.example/knomit")
	rec := httptest.NewRecorder()
	iss.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil))
	pc := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(pc) != 1 || pc[0].Path != "/knomit/oauth/idp/" || !pc[0].Secure {
		t.Fatalf("path issuer: %d %+v", rec.Code, pc)
	}
}

// Off by default: no provider, no cookie, no idp routes, no link.
func TestIDP_OffWithoutAProvider(t *testing.T) {
	f := newIDPFixture(t)
	id := f.park(t, f.browser, "read")
	u, _ := url.Parse(f.srv.URL + "/oauth/idp/")
	if len(f.browser.Jar.Cookies(u)) != 0 {
		t.Fatal("a cookie was set with no provider configured")
	}
	resp, page := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize/"+id+"/wait", nil, nil)
	if strings.Contains(page, "/oauth/idp/start/") {
		t.Fatalf("waiting page links to a provider that is not configured:\n%s", page)
	}
	if resp, _ = f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("start with no provider: %d", resp.StatusCode)
	}
}

// With a provider, the waiting page names what is asked and links to the
// sign-in; its requester fields are escaped.
func TestIDP_WaitingPageOffersTheProvider(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	_, ch := pkce()
	q := f.authorizeQuery(ch)
	q.Set("client_id", "app")
	q.Set("redirect_uri", "https://app.example/cb")
	_, page := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize?"+q.Encode(), nil, nil)
	id := newestPending(t, f.store)
	for _, want := range []string{"/oauth/idp/start/" + id, "Sign in with GitHub", "app.example", "App &lt;b&gt;"} {
		if !strings.Contains(page, want) {
			t.Errorf("waiting page lacks %q", want)
		}
	}
	if strings.Contains(page, "App <b>") {
		t.Fatal("client name unescaped")
	}
}

// Nothing secret from the provider survives the flow anywhere knomit
// writes: not a log line, not a page, not control.db (S3, S4). The refresh
// token GitHub sends unasked is included.
func TestIDP_NoProviderSecretOutlivesTheFlow(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	f.fake.SetExpiringTokens(true)
	logs := captureLog(t)
	id := f.park(t, f.browser, "read")
	_, page, cb := f.signIn(t, f.browser, id)
	pages := []string{page}
	resp, p2 := f.decide(t, f.browser, confirmToken(t, page), "approve", nil)
	pages = append(pages, p2)
	_, p3 := f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	pages = append(pages, p3)

	cbURL, _ := url.Parse(cb)
	secrets := append(f.fake.Issued(), cbURL.Query().Get("code"), idpClientSecret)
	if len(secrets) != 4 {
		t.Fatalf("expected an access token, a refresh token, a code and the secret: %d", len(secrets))
	}
	if err := f.store.db.Close(); err != nil { // flush WAL into the file
		t.Fatal(err)
	}
	var dbBytes []byte
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, _ := os.ReadFile(f.dbPath + suffix)
		dbBytes = append(dbBytes, b...)
	}
	for _, s := range secrets {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a log line carries %q…", s[:6])
		}
		for _, p := range pages {
			if strings.Contains(p, s) {
				t.Errorf("a page carries %q…", s[:6])
			}
		}
		if bytes.Contains(dbBytes, []byte(s)) {
			t.Errorf("control.db carries %q…", s[:6])
		}
	}
	if len(f.fake.Revoked()) != 1 {
		t.Fatalf("the provider token must be revoked: %v", f.fake.Revoked())
	}
}

// W9: a family approved through the provider is refused at refresh — and
// revoked — once its id is off the allow list (here: a restart with an
// edited config). A family approved by an operator is untouched.
func TestIDP_RefreshRefusedOnceTheIDLeavesTheList(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	ver, _ := pkce()
	id := f.park(t, f.browser, "read")
	_, page, _ := f.signIn(t, f.browser, id)
	f.decide(t, f.browser, confirmToken(t, page), "approve", nil)
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize/"+id+"/wait", nil, nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	_, viaIDP := f.exchange(t, loc.Query().Get("code"), ver)

	op := f.park(t, f.browser, "read")
	if _, err := f.iss.Approve(context.Background(), op, "laptop", nil, "bridge:uid:501@socket"); err != nil {
		t.Fatal(err)
	}
	resp, _ = f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/authorize/"+op+"/wait", nil, nil)
	loc, _ = url.Parse(resp.Header.Get("Location"))
	_, viaOp := f.exchange(t, loc.Query().Get("code"), ver)

	f.rebuild("github-9") // 583231 removed
	refresh := func(doc map[string]any) (int, map[string]any) {
		return f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {doc["refresh_token"].(string)}, "client_id": {"kb"}})
	}
	if status, doc := refresh(viaIDP); status != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("refresh after removal: %d %v", status, doc)
	}
	if _, err := f.store.LookupAccess(context.Background(), viaIDP["access_token"].(string)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("the family must be revoked, access token: %v", err)
	}
	if status, doc := refresh(viaOp); status != http.StatusOK {
		t.Fatalf("an operator-approved family must refresh: %d %v", status, doc)
	}
}

// --- helpers ----------------------------------------------------------------

func newestPending(t *testing.T, s *Store) string {
	t.Helper()
	list, err := s.ListPending(context.Background())
	if err != nil || len(list) == 0 {
		t.Fatalf("no pending: %v", err)
	}
	newest := list[0]
	for _, p := range list {
		if p.CreatedAt.After(newest.CreatedAt) {
			newest = p
		}
	}
	if len(list) > 1 {
		t.Fatalf("newestPending needs exactly one row, have %d", len(list))
	}
	return newest.ID
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func captureLog(t *testing.T) *logBuf {
	t.Helper()
	b := &logBuf{}
	orig := log.Logger
	log.Logger = zerolog.New(b)
	t.Cleanup(func() { log.Logger = orig })
	return b
}

// Review C1: the consent page's own Referrer-Policy must let a browser send
// the real Origin on its POST — no-referrer makes it "null", and every real
// approval would be refused while tests that set Origin by hand stayed
// green. The fixture's decide now derives Origin from this header.
func TestIDP_ConsentPagePolicyKeepsTheOrigin(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read")
	resp, _, _ := f.signIn(t, f.browser, id)
	pol := resp.Header.Get("Referrer-Policy")
	if browserOrigin(pol, f.origin()) != f.origin() {
		t.Fatalf("consent page Referrer-Policy %q makes a browser POST Origin: null", pol)
	}
	// And nothing on the page leaks its URL (the callback query) off-origin.
	if pol != "strict-origin" && pol != "same-origin" {
		t.Fatalf("consent page Referrer-Policy %q: want strict-origin or same-origin", pol)
	}
}

// gatedProvider holds Identify until released, to open the window between
// a callback's state lookup and its consent page.
type gatedProvider struct {
	*idp.GitHub
	entered, release chan struct{}
}

func (g *gatedProvider) Identify(ctx context.Context, code, v, r string) (idp.Subject, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.GitHub.Identify(ctx, code, v, r)
}

// Review I1 (reproduced by the final reviewer): a start that replaces a
// sign-in whose callback is still in Identify. The replaced callback must
// not produce a token that decides, and it must not drop the NEW sign-in's
// bookkeeping (which prune, walking byID, would then never free).
func TestIDP_StartDuringCallbackSupersedesIt(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	gp := &gatedProvider{GitHub: f.gh, entered: make(chan struct{}), release: make(chan struct{})}
	f.iss = NewIssuer(Options{Issuer: f.srv.URL, Store: f.store, Clients: f.iss.clients, Grants: f.grants,
		WaitTimeout: time.Second, IDP: &IDPOptions{Provider: gp, Allowed: allowedList("github-583231")}})
	f.handler.set(f.iss.Routes())

	id := f.park(t, f.browser, "read")
	resp, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil)
	resp, _ = f.do(t, f.browser, http.MethodGet, resp.Header.Get("Location"), nil, nil)
	cb1 := resp.Header.Get("Location")
	done := make(chan string)
	go func() {
		r, b := f.do(t, f.browser, http.MethodGet, cb1, nil, nil)
		done <- strconv.Itoa(r.StatusCode) + " " + b
	}()
	<-gp.entered
	if r, _ := f.do(t, f.browser, http.MethodGet, f.srv.URL+"/oauth/idp/start/"+id, nil, nil); r.StatusCode != http.StatusFound {
		t.Fatalf("second start: %d", r.StatusCode)
	}
	close(gp.release)
	page := <-done
	if strings.Contains(page, `name="token"`) || !strings.Contains(page, "newer sign-in") {
		t.Fatalf("the replaced callback must not render a deciding page: %.300s", page)
	}
	if got := f.pending(t, id).Decision; got != "" {
		t.Fatalf("row decided %q by the replaced sign-in", got)
	}
	fl := f.iss.idp
	fl.mu.Lock()
	byID, byState := len(fl.byID), len(fl.byState)
	fl.mu.Unlock()
	if byID != 1 || byState != 1 {
		t.Fatalf("after the superseded callback: byID=%d byState=%d; want the second sign-in alone", byID, byState)
	}
}

// Review M4 (re-graded Important: it breaks every decide): browsers send
// Origin without a default port, and an issuer may be written with one.
func TestIDP_OriginIgnoresTheDefaultPort(t *testing.T) {
	gh := idp.NewGitHub("x", config.NewIDPSecret("s"))
	for issuer, want := range map[string]string{
		"https://h.example:443/knomit": "https://h.example",
		"http://localhost:80":          "http://localhost",
		"https://h.example:8443":       "https://h.example:8443",
		"http://127.0.0.1:19280":       "http://127.0.0.1:19280",
	} {
		if got := newIDPFlow(issuer, &IDPOptions{Provider: gh}).origin; got != want {
			t.Errorf("origin of %q = %q, want %q", issuer, got, want)
		}
	}
}

// Review M1 (re-graded Important: the consent page is the defence, so it
// must not overstate): the ceiling is an upper bound, and a subject
// granted before keeps the grants the operator left (R5) — the page says
// so instead of promising the ceiling.
func TestIDP_ConsentPageDoesNotOverstateAccess(t *testing.T) {
	f := newIDPFixture(t, "github-583231")
	id := f.park(t, f.browser, "read write")
	_, page, _ := f.signIn(t, f.browser, id)
	if !strings.Contains(page, "at most") || strings.Contains(page, "granted before") {
		t.Fatalf("first approval page:\n%s", page)
	}
	if err := f.grants.Grant(context.Background(), TokenPrincipal("github-583231"), auth.Read, "op"); err != nil {
		t.Fatal(err)
	}
	id = f.park(t, f.browser, "read write")
	_, page, _ = f.signIn(t, f.browser, id)
	if !strings.Contains(page, "granted before") {
		t.Fatalf("a previously granted subject's page must say its grants stand:\n%s", page)
	}
}
