package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// issuerFixture is the whole issuer in-process: a real control.db, the real
// resolver with the built-in kb client plus one https app client, and an
// httptest server whose URL is the issuer (http://127.0.0.1:<port> is a
// valid loopback issuer). browser never follows redirects, so each 302 is
// asserted, not trusted.
type issuerFixture struct {
	iss     *Issuer
	srv     *httptest.Server
	store   *Store
	grants  *auth.SQLGrants
	clock   *clock
	browser *http.Client
}

func newIssuerFixture(t *testing.T) *issuerFixture {
	t.Helper()
	db := openControlDB(t)
	f := &issuerFixture{clock: &clock{t: time.Unix(1_790_000_000, 0)}, grants: auth.NewSQLGrants(db)}
	f.store = NewStore(db, 2*time.Hour, 14*24*time.Hour)
	f.store.now = f.clock.now
	var h http.Handler
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(f.srv.Close)
	o := config.Defaults().OAuth
	o.Clients = []config.OAuthClient{{ID: "app", Name: "App", RedirectURIs: []string{"https://app.example/cb"}}}
	f.iss = NewIssuer(Options{
		Issuer:      f.srv.URL,
		Store:       f.store,
		Clients:     NewResolver(o.EffectiveClients()),
		Grants:      f.grants,
		WaitTimeout: 2 * time.Second,
	})
	h = f.iss.Routes()
	f.browser = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f
}

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 20) + "-._~" + strings.Repeat("9", 30)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (f *issuerFixture) authorizeQuery(challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {"kb"},
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
		"scope":                 {"read write"},
		"resource":              {f.srv.URL},
	}
}

func (f *issuerFixture) get(t *testing.T, path string, q url.Values) *http.Response {
	t.Helper()
	u := f.srv.URL + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "fixture-browser/1.0")
	resp, err := f.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// authorize parks a request and returns its pending id.
func (f *issuerFixture) authorize(t *testing.T, q url.Values) string {
	t.Helper()
	before := pendingIDs(t, f.store)
	resp := f.get(t, "/oauth/authorize", q)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: %d %s", resp.StatusCode, body(t, resp))
	}
	return newPendingID(t, before, pendingIDs(t, f.store))
}

// pendingIDs and newPendingID find the request a call just parked. Rows
// created in the same second sort by their RANDOM id, so "the last row" is
// not "the newest row".
func pendingIDs(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	list, err := s.ListPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, p := range list {
		out[p.ID] = true
	}
	return out
}

func newPendingID(t *testing.T, before, after map[string]bool) string {
	t.Helper()
	var found []string
	for id := range after {
		if !before[id] {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one new pending request, got %v", found)
	}
	return found[0]
}

// approveAndCollect runs the whole browser leg and returns the code.
func (f *issuerFixture) approveAndCollect(t *testing.T, q url.Values, scopes []string) string {
	t.Helper()
	id := f.authorize(t, q)
	if _, err := f.iss.Approve(context.Background(), id, "laptop", scopes, "bridge:uid:501@socket"); err != nil {
		t.Fatal(err)
	}
	resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("wait: %d %s", resp.StatusCode, body(t, resp))
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return loc.Query().Get("code")
}

func (f *issuerFixture) token(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(f.srv.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("token response Cache-Control = %q, want no-store", cc)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return resp.StatusCode, doc
}

func (f *issuerFixture) exchange(t *testing.T, code, verifier string) (int, map[string]any) {
	return f.token(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"client_id": {"kb"}, "redirect_uri": {"http://127.0.0.1:54321/callback"},
	})
}

// --- /oauth/authorize --------------------------------------------------------

// Before the client and its redirect URI have validated, nothing may be sent
// to the redirect URI: an error is a page, never a redirect.
func TestAuthorize_ErrorsBeforeRedirectValidatesArePages(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	for name, mut := range map[string]func(url.Values){
		"unknown client":         func(q url.Values) { q.Set("client_id", "nobody") },
		"missing redirect_uri":   func(q url.Values) { q.Del("redirect_uri") },
		"unregistered redirect":  func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") },
		"localhost not wildcard": func(q url.Values) { q.Set("redirect_uri", "http://localhost:54321/callback") },
		"duplicate client_id":    func(q url.Values) { q.Add("client_id", "kb") },
		"duplicate redirect_uri": func(q url.Values) { q.Add("redirect_uri", "http://127.0.0.1:1/callback") },
	} {
		t.Run(name, func(t *testing.T) {
			q := f.authorizeQuery(ch)
			mut(q)
			resp := f.get(t, "/oauth/authorize", q)
			if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
				t.Fatalf("status %d Location %q: want a 400 page and no redirect", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
	if list, _ := f.store.ListPending(context.Background()); len(list) != 0 {
		t.Fatalf("a refused request was parked: %+v", list)
	}
}

// After they validate, every error goes to the redirect URI with state and
// iss (RFC 9207), and nothing is parked.
func TestAuthorize_ErrorsAfterRedirectValidatesAreRedirects(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	for name, tc := range map[string]struct {
		mut  func(url.Values)
		want string
	}{
		"response_type token":  {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		"no challenge":         {func(q url.Values) { q.Del("code_challenge") }, "invalid_request"},
		"no method":            {func(q url.Values) { q.Del("code_challenge_method") }, "invalid_request"},
		"plain method":         {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"short challenge":      {func(q url.Values) { q.Set("code_challenge", "abc") }, "invalid_request"},
		"no resource":          {func(q url.Values) { q.Del("resource") }, "invalid_target"},
		"foreign resource":     {func(q url.Values) { q.Set("resource", "https://other.example") }, "invalid_target"},
		"resource escapes":     {func(q url.Values) { q.Set("resource", f.srv.URL+"/api/../x") }, "invalid_target"},
		"resource with query":  {func(q url.Values) { q.Set("resource", f.srv.URL+"/api?x=1") }, "invalid_target"},
		"resource host suffix": {func(q url.Values) { q.Set("resource", f.srv.URL+"0/api") }, "invalid_target"},
		"resource other host":  {func(q url.Values) { q.Set("resource", strings.Replace(f.srv.URL, "127.0.0.1", "127.0.0.2", 1)) }, "invalid_target"},
		"admin scope":          {func(q url.Values) { q.Set("scope", "read admin") }, "invalid_scope"},
		"unknown scope":        {func(q url.Values) { q.Set("scope", "everything") }, "invalid_scope"},
		"duplicate state":      {func(q url.Values) { q.Add("state", "again") }, "invalid_request"},
		// A repeated OPTIONAL parameter must not be dropped and the request
		// parked as if it had not been sent.
		"duplicate scope": {func(q url.Values) { q.Add("scope", "read") }, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			q := f.authorizeQuery(ch)
			tc.mut(q)
			resp := f.get(t, "/oauth/authorize", q)
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("status %d: want a redirect; body %s", resp.StatusCode, body(t, resp))
			}
			loc, err := url.Parse(resp.Header.Get("Location"))
			if err != nil || loc.Host != "127.0.0.1:54321" || loc.Path != "/callback" {
				t.Fatalf("Location %q", resp.Header.Get("Location"))
			}
			lq := loc.Query()
			if lq.Get("error") != tc.want {
				t.Errorf("error = %q, want %q", lq.Get("error"), tc.want)
			}
			if lq.Get("iss") != f.srv.URL {
				t.Errorf("iss = %q, want the issuer", lq.Get("iss"))
			}
			if name != "duplicate state" && lq.Get("state") != "xyz" {
				t.Errorf("state = %q", lq.Get("state"))
			}
		})
	}
	if list, _ := f.store.ListPending(context.Background()); len(list) != 0 {
		t.Fatalf("a refused request was parked: %+v", list)
	}
}

func TestAuthorize_ParksAndNamesTheRequest(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	q := f.authorizeQuery(ch)
	q.Set("resource", f.srv.URL+"/api/v1/repos/r/mcp") // a resource under the issuer
	resp := f.get(t, "/oauth/authorize", q)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, page)
	}
	list, _ := f.store.ListPending(context.Background())
	if len(list) != 1 {
		t.Fatalf("pending = %+v", list)
	}
	p := list[0]
	if !strings.Contains(page, p.ID) {
		t.Fatal("the waiting page does not name the request id the operator approves")
	}
	if !strings.Contains(page, f.srv.URL+"/oauth/authorize/"+p.ID+"/wait") {
		t.Fatal("the waiting page does not lead to /wait")
	}
	if p.UserAgent != "fixture-browser/1.0" || !strings.HasPrefix(p.RemoteAddr, "127.0.0.1:") ||
		p.ClientName != "kb (knomit bridge)" || p.Resource != f.srv.URL+"/api/v1/repos/r/mcp" {
		t.Fatalf("pending row = %+v", p)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("waiting page must not be cached")
	}
}

// --- approve, deny, wait ------------------------------------------------------

func TestApprove_IssuesCodeBoundToTheRequestAndWritesGrants(t *testing.T) {
	f := newIssuerFixture(t)
	ctx := context.Background()
	ver, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	p, err := f.iss.Approve(ctx, id, "laptop", nil, "bridge:uid:501@socket")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Ceiling, " ") != "read write" {
		t.Fatalf("default ceiling = %v, want requested ∩ {read, write}", p.Ceiling)
	}
	resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("wait: %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "127.0.0.1:54321" || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != f.srv.URL {
		t.Fatalf("redirect %q", loc)
	}
	code := loc.Query().Get("code")
	status, doc := f.exchange(t, code, ver)
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, doc)
	}
	if doc["scope"] != "read write" || doc["token_type"] != "Bearer" {
		t.Fatalf("token response %v", doc)
	}
	principal := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	set, err := f.grants.For(ctx, principal)
	if err != nil || !set.Has(auth.Read) || !set.Has(auth.Write) || set.Has(auth.Admin) {
		t.Fatalf("grants for %s = %v, %v", principal, set, err)
	}
	rows, _ := f.grants.List(ctx, principal.String())
	if len(rows) == 0 || rows[0].GrantedBy != "bridge:uid:501@socket" {
		t.Fatalf("grants rows must name the approver: %+v", rows)
	}
}

func TestApprove_CeilingRules(t *testing.T) {
	f := newIssuerFixture(t)
	ctx := context.Background()
	_, ch := pkce()

	q := f.authorizeQuery(ch)
	q.Set("scope", "")
	id := f.authorize(t, q)
	p, err := f.iss.Approve(ctx, id, "laptop", nil, "x")
	if err != nil || strings.Join(p.Ceiling, " ") != "read" {
		t.Fatalf("nothing requested: ceiling %v, %v; want read", p.Ceiling, err)
	}

	// Only read and write are granted by default, whatever else was asked.
	q = f.authorizeQuery(ch)
	q.Set("scope", "operator write push:own")
	id = f.authorize(t, q)
	if p, err := f.iss.Approve(ctx, id, "laptop", nil, "x"); err != nil || strings.Join(p.Ceiling, " ") != "write" {
		t.Fatalf("operator+write+push:own requested: ceiling %v, %v; want write", p.Ceiling, err)
	}

	// An explicit --scopes may grant beyond read and write (never admin),
	// and comes back in one canonical order.
	id = f.authorize(t, f.authorizeQuery(ch))
	if p, err := f.iss.Approve(ctx, id, "laptop", []string{"operator", "read"}, "x"); err != nil || strings.Join(p.Ceiling, " ") != "read operator" {
		t.Fatalf("--scopes operator,read: %v, %v", p.Ceiling, err)
	}

	id = f.authorize(t, f.authorizeQuery(ch))
	if p, err := f.iss.Approve(ctx, id, "laptop", []string{"read"}, "x"); err != nil || strings.Join(p.Ceiling, " ") != "read" {
		t.Fatalf("--scopes read: %v, %v", p.Ceiling, err)
	}

	for _, bad := range [][]string{{"admin"}, {"read", "admin"}, {"everything"}, {}} {
		id = f.authorize(t, f.authorizeQuery(ch))
		if _, err := f.iss.Approve(ctx, id, "laptop", bad, "x"); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("--scopes %v: want ErrInvalidScope, got %v", bad, err)
		}
	}
}

func TestApprove_SubjectMustBeAPrincipalID(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	for _, bad := range []string{"", " laptop", "lap top", "a\nb", strings.Repeat("x", 129)} {
		id := f.authorize(t, f.authorizeQuery(ch))
		if _, err := f.iss.Approve(context.Background(), id, bad, nil, "x"); !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("subject %q: want ErrInvalidSubject, got %v", bad, err)
		}
	}
	id := f.authorize(t, f.authorizeQuery(ch))
	if _, err := f.iss.Approve(context.Background(), id, "ci-runner@example.org", nil, "x"); err != nil {
		t.Fatalf("an email-shaped subject is a valid id: %v", err)
	}
}

func TestDeny_RedirectsAccessDenied(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	if err := f.iss.Deny(context.Background(), id, "x"); err != nil {
		t.Fatal(err)
	}
	resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("error") != "access_denied" ||
		loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != f.srv.URL || loc.Query().Get("code") != "" {
		t.Fatalf("deny: %d %q", resp.StatusCode, loc)
	}
}

func TestApprove_ExpiredRefused(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	f.clock.add(pendingTTL)
	if _, err := f.iss.Approve(context.Background(), id, "laptop", nil, "x"); !errors.Is(err, ErrExpired) {
		t.Fatalf("approving an expired request: %v", err)
	}
	principal := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	if set, _ := f.grants.For(context.Background(), principal); len(set) != 0 {
		t.Fatalf("a refused approval wrote grants: %v", set)
	}
}

// /wait is a long-poll: a browser already waiting is answered as soon as
// the operator decides, not at the end of the window.
func TestWait_LongPollWakesOnApproval(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	done := make(chan *http.Response, 1)
	start := time.Now()
	go func() {
		resp, err := f.browser.Get(f.srv.URL + "/oauth/authorize/" + id + "/wait")
		if err != nil {
			t.Error(err)
			close(done)
			return
		}
		done <- resp
	}()
	time.Sleep(200 * time.Millisecond) // let the poll park
	if _, err := f.iss.Approve(context.Background(), id, "laptop", nil, "x"); err != nil {
		t.Fatal(err)
	}
	resp := <-done
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Fatalf("the poll waited %v; it must wake on the decision, not at the %v window", el, 2*time.Second)
	}
}

func TestWait_UndecidedReturnsTheWaitingPageAgain(t *testing.T) {
	f := newIssuerFixture(t)
	f.iss.waitTimeout = 100 * time.Millisecond
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil)
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, id) || !strings.Contains(page, "/wait") {
		t.Fatalf("undecided wait: %d %s", resp.StatusCode, page)
	}
}

func TestWait_OnceOnlyAndUnknown(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	f.approveAndCollect(t, f.authorizeQuery(ch), nil)
	list, _ := f.store.db.Query(`SELECT id FROM oauth_pending`)
	var id string
	list.Next()
	_ = list.Scan(&id)
	list.Close()
	if resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil); resp.StatusCode != http.StatusGone {
		t.Fatalf("second collection: %d", resp.StatusCode)
	}
	if resp := f.get(t, "/oauth/authorize/nope/wait", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id: %d", resp.StatusCode)
	}
}

// An expired request, once the browser comes back, is answered on the
// redirect URI (which validated when it was parked) as access_denied.
func TestWait_ExpiredIsAccessDenied(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	f.clock.add(pendingTTL)
	resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("error") != "access_denied" || loc.Query().Get("iss") != f.srv.URL {
		t.Fatalf("expired: %d %q", resp.StatusCode, loc)
	}
}

// --- /oauth/token ------------------------------------------------------------

func TestToken_CodeExchange(t *testing.T) {
	f := newIssuerFixture(t)
	ver, ch := pkce()
	code := f.approveAndCollect(t, f.authorizeQuery(ch), []string{"read"})
	status, doc := f.exchange(t, code, ver)
	if status != http.StatusOK {
		t.Fatalf("%d %v", status, doc)
	}
	if doc["token_type"] != "Bearer" || doc["scope"] != "read" || doc["access_token"] == "" || doc["refresh_token"] == "" {
		t.Fatalf("response %v", doc)
	}
	if exp, _ := doc["expires_in"].(float64); exp != 7200 {
		t.Fatalf("expires_in = %v, want 7200", doc["expires_in"])
	}
	fam, err := f.store.LookupAccess(context.Background(), doc["access_token"].(string))
	if err != nil || fam.Subject != "laptop" || fam.Resource != f.srv.URL {
		t.Fatalf("issued token: %+v %v", fam, err)
	}
}

func TestToken_CodeExchangeRefusals(t *testing.T) {
	ver, ch := pkce()
	for name, tc := range map[string]struct {
		mut  func(f *issuerFixture, form url.Values)
		want string
	}{
		"verifier mismatch":     {func(_ *issuerFixture, v url.Values) { v.Set("code_verifier", strings.Repeat("w", 43)) }, "invalid_grant"},
		"verifier missing":      {func(_ *issuerFixture, v url.Values) { v.Del("code_verifier") }, "invalid_request"},
		"verifier too short":    {func(_ *issuerFixture, v url.Values) { v.Set("code_verifier", strings.Repeat("v", 42)) }, "invalid_request"},
		"verifier bad chars":    {func(_ *issuerFixture, v url.Values) { v.Set("code_verifier", strings.Repeat("v", 42)+"!") }, "invalid_request"},
		"redirect_uri mismatch": {func(_ *issuerFixture, v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:1/callback") }, "invalid_grant"},
		"client mismatch":       {func(_ *issuerFixture, v url.Values) { v.Set("client_id", "app") }, "invalid_grant"},
		"resource mismatch":     {func(f *issuerFixture, v url.Values) { v.Set("resource", f.srv.URL+"/api/v1/other") }, "invalid_target"},
		"unknown code":          {func(_ *issuerFixture, v url.Values) { v.Set("code", "nope") }, "invalid_grant"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newIssuerFixture(t)
			code := f.approveAndCollect(t, f.authorizeQuery(ch), nil)
			form := url.Values{
				"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {ver},
				"client_id": {"kb"}, "redirect_uri": {"http://127.0.0.1:54321/callback"},
			}
			tc.mut(f, form)
			status, doc := f.token(t, form)
			if status != http.StatusBadRequest || doc["error"] != tc.want {
				t.Fatalf("%d %v; want 400 %s", status, doc, tc.want)
			}
		})
	}
}

func TestToken_CodeReuseRevokesFamily(t *testing.T) {
	f := newIssuerFixture(t)
	ver, ch := pkce()
	code := f.approveAndCollect(t, f.authorizeQuery(ch), nil)
	_, first := f.exchange(t, code, ver)
	status, doc := f.exchange(t, code, ver)
	if status != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("replay: %d %v", status, doc)
	}
	if _, err := f.store.LookupAccess(context.Background(), first["access_token"].(string)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("replay must revoke the first exchange's tokens: %v", err)
	}
}

func TestToken_RefreshRotationAndReuse(t *testing.T) {
	f := newIssuerFixture(t)
	ver, ch := pkce()
	_, first := f.exchange(t, f.approveAndCollect(t, f.authorizeQuery(ch), nil), ver)
	refresh := func(tok string, extra url.Values) (int, map[string]any) {
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {"kb"}}
		for k, v := range extra {
			form[k] = v
		}
		return f.token(t, form)
	}
	// A refresh that omits resource reuses the family's (Claude Code does).
	status, second := refresh(first["refresh_token"].(string), nil)
	if status != http.StatusOK || second["refresh_token"] == first["refresh_token"] {
		t.Fatalf("rotation: %d %v", status, second)
	}
	if status, doc := refresh(second["refresh_token"].(string), url.Values{"resource": {f.srv.URL + "/api/v1/x"}}); status != http.StatusBadRequest || doc["error"] != "invalid_target" {
		t.Fatalf("other resource: %d %v", status, doc)
	}
	if status, doc := refresh(second["refresh_token"].(string), url.Values{"client_id": {"app"}}); status != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("other client: %d %v", status, doc)
	}
	if status, doc := refresh(second["refresh_token"].(string), url.Values{"scope": {"read write admin"}}); status != http.StatusBadRequest || doc["error"] != "invalid_scope" {
		t.Fatalf("widening scope: %d %v", status, doc)
	}
	// The old one again: the family dies, including the pair just issued.
	if status, doc := refresh(first["refresh_token"].(string), nil); status != http.StatusBadRequest || doc["error"] != "invalid_grant" {
		t.Fatalf("reuse: %d %v", status, doc)
	}
	if _, err := f.store.LookupAccess(context.Background(), second["access_token"].(string)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("reuse must revoke the new access token: %v", err)
	}
}

func TestToken_BadRequests(t *testing.T) {
	f := newIssuerFixture(t)
	if status, doc := f.token(t, url.Values{"grant_type": {"password"}}); status != http.StatusBadRequest || doc["error"] != "unsupported_grant_type" {
		t.Fatalf("password grant: %d %v", status, doc)
	}
	if status, doc := f.token(t, url.Values{"grant_type": {"refresh_token"}, "client_id": {"kb"}}); status != http.StatusBadRequest || doc["error"] != "invalid_request" {
		t.Fatalf("no refresh_token: %d %v", status, doc)
	}
	if status, doc := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"a"}, "client_id": {"kb", "app"}}); status != http.StatusBadRequest || doc["error"] != "invalid_request" {
		t.Fatalf("repeated client_id: %d %v", status, doc)
	}
	resp, _ := http.Get(f.srv.URL + "/oauth/token")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /oauth/token: %d", resp.StatusCode)
	}
}

// --- /oauth/revoke (RFC 7009) -------------------------------------------------

func TestRevoke_IdempotentAndKillsTheFamily(t *testing.T) {
	f := newIssuerFixture(t)
	ver, ch := pkce()
	_, doc := f.exchange(t, f.approveAndCollect(t, f.authorizeQuery(ch), nil), ver)
	for i := 0; i < 2; i++ {
		resp, err := http.PostForm(f.srv.URL+"/oauth/revoke", url.Values{"token": {doc["refresh_token"].(string)}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("revoke #%d: %d", i+1, resp.StatusCode)
		}
	}
	if _, err := f.store.LookupAccess(context.Background(), doc["access_token"].(string)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoking the refresh token must kill the access token: %v", err)
	}
	resp, _ := http.PostForm(f.srv.URL+"/oauth/revoke", url.Values{"token": {"never-issued"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown token: %d", resp.StatusCode)
	}
	resp, _ = http.PostForm(f.srv.URL+"/oauth/revoke", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func (f *issuerFixture) waiterEntries() int {
	f.iss.waiters.mu.Lock()
	defer f.iss.waiters.mu.Unlock()
	return len(f.iss.waiters.m)
}

// /wait is public on the proxied listener: an id nobody parked must cost
// nothing that outlives the request — not a map entry, whatever its length
// (review B1: 5000 unknown ids left 5000 entries; 512 KiB ids pinned 100 MiB).
func TestWait_UnknownIDsLeaveNoWaiter(t *testing.T) {
	f := newIssuerFixture(t)
	for i := 0; i < 50; i++ {
		id := strings.Repeat("A", 43) // the pending-id shape, but never parked
		if i%2 == 1 {
			id = strings.Repeat("x", 4096) // not the shape at all
		}
		if resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown id: %d", resp.StatusCode)
		}
	}
	if n := f.waiterEntries(); n != 0 {
		t.Fatalf("%d waiter entries after unknown ids, want 0", n)
	}
}

// A known, undecided id registers exactly one entry while a browser waits,
// and the entry is gone once that wait ends — by a decision, by the window
// closing, or by the request having expired.
func TestWait_KnownIDOneEntryRemovedAfter(t *testing.T) {
	f := newIssuerFixture(t)
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	done := make(chan struct{})
	go func() {
		resp, err := f.browser.Get(f.srv.URL + "/oauth/authorize/" + id + "/wait")
		if err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for f.waiterEntries() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.waiterEntries(); n != 1 {
		t.Fatalf("a parked wait holds %d entries, want 1", n)
	}
	if _, err := f.iss.Approve(context.Background(), id, "laptop", nil, "x"); err != nil {
		t.Fatal(err)
	}
	<-done
	if n := f.waiterEntries(); n != 0 {
		t.Fatalf("after the decision: %d entries, want 0", n)
	}

	// The window closing with no decision also removes it.
	f.iss.waitTimeout = 50 * time.Millisecond
	id2 := f.authorize(t, f.authorizeQuery(ch))
	f.get(t, "/oauth/authorize/"+id2+"/wait", nil)
	if n := f.waiterEntries(); n != 0 {
		t.Fatalf("after an undecided window: %d entries, want 0", n)
	}
	// An expired request never registers one.
	f.clock.add(pendingTTL)
	f.get(t, "/oauth/authorize/"+id2+"/wait", nil)
	if n := f.waiterEntries(); n != 0 {
		t.Fatalf("after an expired request: %d entries, want 0", n)
	}
}

// A decision that lands between /wait's first read and its registration
// notified nobody; the re-read after registering still sees it, so the
// browser is answered at once rather than at the end of the window.
func TestWait_DecisionBetweenReadAndRegisterIsSeen(t *testing.T) {
	f := newIssuerFixture(t)
	f.iss.waitTimeout = 5 * time.Second
	_, ch := pkce()
	id := f.authorize(t, f.authorizeQuery(ch))
	f.iss.beforeRegister = func(got string) {
		f.iss.beforeRegister = nil
		if _, err := f.iss.Approve(context.Background(), got, "laptop", nil, "x"); err != nil {
			t.Error(err)
		}
	}
	start := time.Now()
	resp := f.get(t, "/oauth/authorize/"+id+"/wait", nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("answered after %v: the decision in the gap was missed until the window closed", el)
	}
}
