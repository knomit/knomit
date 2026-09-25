package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// The browser branch of the approval gate (F19 phase 3b, Task 1, rulings
// R2/R3 and the master's own-origin correction): a request from the web UI
// served on the plain listener may list, approve and deny iff it is the
// anonymous loopback principal holding admin AND it carries the browser
// proof. Every case below that must be refused is refused with 403 and a
// reason naming the missing proof, and leaves the request undecided.

const gatePort = "19480"

// browserReq builds what the web UI's fetch sends from http://<host>:19480:
// the listener's local address in the context (as net/http sets it), a
// loopback peer, the custom header, Sec-Fetch-Site, and for a mutation
// Origin and a JSON body.
func browserReq(method, path, body, host string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 19480}))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = host + ":" + gatePort
	r.Header.Set(KnomitClientHeader, KnomitClientWeb)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", "http://"+host+":"+gatePort)
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func approveBody() string { return `{"subject":"laptop","scopes":["read"]}` }

func TestBrowserGate_ProofAdmitsListApproveDeny(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			f := newKBFixture(t)
			h := f.s.Handler()
			if rec := serve(h, browserReq(http.MethodGet, "/api/v1/oauth/pending", "", host)); rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			id := f.park(t)
			rec := serve(h, browserReq(http.MethodPost, "/api/v1/oauth/pending/"+id+"/approve", approveBody(), host))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"decision":"approved"`) {
				t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
			}
			rows, _ := f.grants.List(context.Background(), oauth.TokenPrincipal("laptop").String())
			if len(rows) != 1 || rows[0].GrantedBy != "anonymous@none" {
				t.Fatalf("grants must name the approving browser principal: %+v", rows)
			}
			id2 := f.park(t)
			if rec := serve(h, browserReq(http.MethodPost, "/api/v1/oauth/pending/"+id2+"/deny", "", host)); rec.Code != http.StatusNoContent {
				t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A Content-Type with parameters is still JSON; the listener on port 80
// matches a Host with no port.
func TestBrowserGate_Normalisations(t *testing.T) {
	f := newKBFixture(t)
	h := f.s.Handler()
	id := f.park(t)
	r := browserReq(http.MethodPost, "/api/v1/oauth/pending/"+id+"/deny", "", "localhost")
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if rec := serve(h, r); rec.Code != http.StatusNoContent {
		t.Fatalf("json with charset: %d %s", rec.Code, rec.Body.String())
	}
	r = browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "localhost")
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}))
	r.Host = "localhost"
	if rec := serve(h, r); rec.Code != http.StatusOK {
		t.Fatalf("port 80, Host without port: %d %s", rec.Code, rec.Body.String())
	}
	// Sec-Fetch-Site absent (an older browser, or curl) is allowed when
	// everything else holds.
	r = browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "localhost")
	r.Header.Del("Sec-Fetch-Site")
	if rec := serve(h, r); rec.Code != http.StatusOK {
		t.Fatalf("no Sec-Fetch-Site: %d %s", rec.Code, rec.Body.String())
	}
}

// Each proof component, removed or forged alone, refuses — with a reason
// that names it. Sabotage: deleting any one check from browserProof fails
// exactly the rows that name it.
func TestBrowserGate_EachMissingProofRefuses(t *testing.T) {
	type mut func(r *http.Request)
	get := func(m mut) func(id string) *http.Request {
		return func(string) *http.Request {
			r := browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "127.0.0.1")
			m(r)
			return r
		}
	}
	post := func(m mut) func(id string) *http.Request {
		return func(id string) *http.Request {
			r := browserReq(http.MethodPost, "/api/v1/oauth/pending/"+id+"/approve", approveBody(), "127.0.0.1")
			m(r)
			return r
		}
	}
	cases := []struct {
		name   string
		req    func(id string) *http.Request
		reason string
	}{
		{"GET without X-Knomit-Client", get(func(r *http.Request) { r.Header.Del(KnomitClientHeader) }), KnomitClientHeader},
		{"GET with X-Knomit-Client: other", get(func(r *http.Request) { r.Header.Set(KnomitClientHeader, "cli") }), KnomitClientHeader},
		{"POST without X-Knomit-Client", post(func(r *http.Request) { r.Header.Del(KnomitClientHeader) }), KnomitClientHeader},
		// DNS rebinding: the attacker's name resolves to 127.0.0.1, so the
		// page is same-origin with itself and the browser says so.
		{"rebinding", post(func(r *http.Request) {
			r.Host = "evil.example:" + gatePort
			r.Header.Set("Origin", "http://evil.example:"+gatePort)
		}), "Host"},
		{"rebinding GET", get(func(r *http.Request) { r.Host = "evil.example:" + gatePort }), "Host"},
		{"Host 127.0.0.2", get(func(r *http.Request) { r.Host = "127.0.0.2:" + gatePort }), "Host"},
		{"Host 127.0.0.1.nip.io", get(func(r *http.Request) { r.Host = "127.0.0.1.nip.io:" + gatePort }), "Host"},
		{"Host localhost.", get(func(r *http.Request) { r.Host = "localhost.:" + gatePort }), "Host"},
		{"Host sub.localhost", get(func(r *http.Request) { r.Host = "app.localhost:" + gatePort }), "Host"},
		{"Host another port", get(func(r *http.Request) { r.Host = "127.0.0.1:3000" }), "Host"},
		{"Host without port on 19480", get(func(r *http.Request) { r.Host = "127.0.0.1" }), "Host"},
		{"Origin absent on a mutation", post(func(r *http.Request) { r.Header.Del("Origin") }), "Origin"},
		{"Origin null", post(func(r *http.Request) { r.Header.Set("Origin", "null") }), "Origin"},
		{"Origin another port (same-site)", post(func(r *http.Request) {
			r.Header.Set("Origin", "http://127.0.0.1:3000")
			r.Header.Set("Sec-Fetch-Site", "same-site")
		}), "Sec-Fetch-Site"}, // an honest browser says same-site; the next row lies
		{"Origin another port, Sec-Fetch-Site lying", post(func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:3000") }), "Origin"},
		{"Origin https", post(func(r *http.Request) { r.Header.Set("Origin", "https://127.0.0.1:"+gatePort) }), "Origin"},
		{"Origin other loopback spelling", post(func(r *http.Request) { r.Header.Set("Origin", "http://localhost:"+gatePort) }), "Origin"},
		{"Origin wails (desktop allowlist is never own origin)", post(func(r *http.Request) { r.Header.Set("Origin", "wails://localhost") }), "Origin"},
		{"Origin mismatched on GET", get(func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }), "Origin"},
		{"Sec-Fetch-Site cross-site", post(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }), "Sec-Fetch-Site"},
		{"Sec-Fetch-Site same-site", get(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }), "Sec-Fetch-Site"},
		{"Sec-Fetch-Site none", get(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") }), "Sec-Fetch-Site"},
		// A cross-site HTML form can send only these three types, and no
		// custom header; the type alone must refuse it.
		{"form POST", post(func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }), "Content-Type"},
		{"text/plain POST", post(func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }), "Content-Type"},
		{"no Content-Type", post(func(r *http.Request) { r.Header.Del("Content-Type") }), "Content-Type"},
		{"non-loopback peer", get(func(r *http.Request) { r.RemoteAddr = "192.0.2.1:5000" }), ""},
	}
	// These rows are 421, not 403. Their Host is a DNS name that is not
	// localhost, so since #281 AuthMiddleware refuses them one step EARLIER,
	// before any principal exists, and the request never reaches this gate.
	// They stay here because the property this test pins still holds:
	// refused, the reason names the Host, and the request stays undecided.
	// The rows whose Host is an IP literal or the wrong port pass
	// AuthMiddleware and still meet this gate's 403.
	refusedEarlier := map[string]bool{
		"rebinding": true, "rebinding GET": true, "Host 127.0.0.1.nip.io": true,
		"Host localhost.": true, "Host sub.localhost": true,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newKBFixture(t)
			id := f.park(t)
			want := http.StatusForbidden
			if refusedEarlier[c.name] {
				want = http.StatusMisdirectedRequest
			}
			rec := serve(f.s.Handler(), c.req(id))
			if rec.Code != want {
				t.Fatalf("%d %s; want %d", rec.Code, rec.Body.String(), want)
			}
			if !strings.Contains(rec.Body.String(), c.reason) {
				t.Fatalf("%d body %s does not name %q", want, rec.Body.String(), c.reason)
			}
			if list, _ := f.s.OAuthIssuer.Pending(context.Background()); len(list) != 1 || list[0].Decision != "" {
				t.Fatalf("a refused request decided it: %+v", list)
			}
		})
	}
}

// R3: the browser branch is for anonymous@none on the plain listener only.
// A certificate principal holding admin by row, arriving over the TLS
// listener (which serves the same Handler) with every browser header, is
// refused; so is a bearer principal, at the gate and — since the route is
// not on the OAuth router at all — as a 404 there.
func TestBrowserGate_OnlyAnonymousLoopback(t *testing.T) {
	f := newKBFixture(t)
	id := f.park(t)
	pk := pkitest.New(t)
	op := pk.Enroll(t, "op", pki.RoleOperator)
	opP := auth.OperatorPrincipal(op.Fingerprint())
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := f.grants.Grant(context.Background(), opP, perm, "test"); err != nil {
			t.Fatal(err)
		}
	}
	r := browserReq(http.MethodPost, "/api/v1/oauth/pending/"+id+"/deny", "", "127.0.0.1")
	if rec := serve(asTLSPeer(f.s.Handler(), op.Cert), r); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), opP.String()) {
		t.Fatalf("operator certificate with admin and every browser proof: %d %s; want 403 naming it", rec.Code, rec.Body.String())
	}

	// The gate itself, handed a bearer principal whose grants include admin.
	tokP := oauth.TokenPrincipal("admin-token")
	for _, perm := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		if err := f.grants.Grant(context.Background(), tokP, perm, "test"); err != nil {
			t.Fatal(err)
		}
	}
	reached := false
	gate := f.s.requireLocalOrSameOriginAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r = browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "127.0.0.1")
	r = r.WithContext(auth.WithCeiling(auth.WithPrincipal(r.Context(), tokP), auth.Set{auth.Read: {}, auth.Write: {}, auth.Admin: {}}))
	rec := httptest.NewRecorder()
	gate.ServeHTTP(rec, r)
	if reached || rec.Code != http.StatusForbidden {
		t.Fatalf("bearer principal at the gate: reached=%v %d %s", reached, rec.Code, rec.Body.String())
	}

	// On the OAuth listener the route does not exist.
	tok := f.mint(t, "admin-token", testIssuer, []string{"read", "write"}, auth.Read, auth.Write, auth.Admin)
	r = browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "127.0.0.1")
	r.Header.Set("Authorization", "Bearer "+tok)
	if rec := serve(f.s.OAuthHandler(), r); rec.Code != http.StatusNotFound {
		t.Fatalf("pending on the OAuth router: %d %s; want 404", rec.Code, rec.Body.String())
	}
}

// The gate's own belts, below AuthMiddleware: anonymous@none arriving over
// the TLS listener, or from a non-loopback peer, is refused even with every
// browser proof. AuthMiddleware never produces either today (so a test
// through Handler cannot see these checks); this pins them for the day it
// might. Sabotage: dropping IsTLSListener or isLoopback from the gate fails
// the matching row, and nothing else does.
func TestBrowserGate_BeltsBelowAuthMiddleware(t *testing.T) {
	f := newKBFixture(t)
	anon := auth.Principal{Kind: auth.KindAnonymous, Via: auth.ViaNone}
	for name, mut := range map[string]func(r *http.Request) *http.Request{
		"TLS listener": func(r *http.Request) *http.Request { return r.WithContext(auth.TLSConnContext(r.Context(), nil)) },
		"non-loopback": func(r *http.Request) *http.Request { r.RemoteAddr = "192.0.2.1:5000"; return r },
	} {
		t.Run(name, func(t *testing.T) {
			reached := false
			gate := f.s.requireLocalOrSameOriginAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			r := browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "127.0.0.1")
			r = mut(r.WithContext(auth.WithPrincipal(r.Context(), anon)))
			rec := httptest.NewRecorder()
			gate.ServeHTTP(rec, r)
			if reached || rec.Code != http.StatusForbidden {
				t.Fatalf("reached=%v %d %s", reached, rec.Code, rec.Body.String())
			}
		})
	}
	// Control: the same call, unmutated, passes — so the rows above fail
	// on the belt and not on a broken fixture.
	reached := false
	gate := f.s.requireLocalOrSameOriginAdmin(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r := browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "127.0.0.1")
	gate.ServeHTTP(httptest.NewRecorder(), r.WithContext(auth.WithPrincipal(r.Context(), anon)))
	if !reached {
		t.Fatal("control: anonymous@none with every proof did not pass the gate")
	}
}

// S7: anonymous loopback without admin (loopback_default narrowed by the
// operator) has no browser path at all.
func TestBrowserGate_AnonymousWithoutAdminRefused(t *testing.T) {
	f := newKBFixture(t)
	f.s.Auth = config.AuthConfig{LoopbackDefault: []string{"read", "write"}}
	rec := serve(f.s.Handler(), browserReq(http.MethodGet, "/api/v1/oauth/pending", "", "127.0.0.1"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin") {
		t.Fatalf("%d %s; want 403 naming admin", rec.Code, rec.Body.String())
	}
}

// The socket path is unchanged: a local principal needs no browser proof.
func TestBrowserGate_LocalPrincipalNeedsNoProof(t *testing.T) {
	f := newKBFixture(t)
	rec := serve(f.s.Handler(), localPeer(httptest.NewRequest(http.MethodGet, "/api/v1/oauth/pending", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("socket list: %d %s", rec.Code, rec.Body.String())
	}
}

// R2: the custom header forces a CORS preflight on any cross-origin caller.
// On `knomit serve` (no CORS middleware) that preflight is refused — 403
// from the gate, which chi runs before method dispatch, so never the 405 R2
// anticipated; what makes it a refusal is the non-2xx status and the absent
// Access-Control-Allow-Origin, and that is what is asserted. On the
// desktop build an allowlisted Wails origin may send the header, and any
// other origin gets no Access-Control-Allow-Origin.
func TestBrowserGate_Preflight(t *testing.T) {
	f := newKBFixture(t)
	pre := func(origin string) *http.Request {
		r := fromLoopback(httptest.NewRequest(http.MethodOptions, "/api/v1/oauth/pending/x/approve", nil))
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "content-type,x-knomit-client")
		return r
	}
	rec := serve(f.s.Handler(), pre("http://evil.example"))
	if rec.Code < 300 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("serve preflight: %d ACAO=%q; want a refusal and no ACAO", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}

	f.s.CORSOrigins = []string{"wails://localhost"}
	rec = serve(f.s.Handler(), pre("wails://localhost"))
	allow := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers"))
	if rec.Header().Get("Access-Control-Allow-Origin") != "wails://localhost" || !strings.Contains(allow, strings.ToLower(KnomitClientHeader)) {
		t.Fatalf("desktop preflight: ACAO=%q Allow-Headers=%q; want the Wails origin and %s", rec.Header().Get("Access-Control-Allow-Origin"), allow, KnomitClientHeader)
	}
	rec = serve(f.s.Handler(), pre("http://evil.example"))
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("desktop preflight from a foreign origin got ACAO %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}
