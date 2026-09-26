package web

// #287: a loopback request that would become the anonymous principal is
// refused with 403 "Cross-origin request refused" when it changes data
// (any method but GET, HEAD, OPTIONS) and carries an Origin that is neither
// the request's own http(s)://<Host> nor in the CORS allowlist. No Origin
// (curl, the CLI, the bridge) passes. The Host check (#281, 421) runs first.
//
// Every end-to-end test here drives the real Server.Handler over a real
// 127.0.0.1 listener, so both AuthMiddleware invocations (outer router and
// API router) are exercised.
//
// Sabotage rows (each applied alone; the named test failed):
//
//	S1 delete the loopbackOriginOK call in authMiddleware   → TestIssue287_CrossSitePlainPostIsRefused
//	S2 thread the allowlist into only ONE of the two edges  → TestOriginGuard_DesktopWailsOriginWrites
//	S3 exempt POST (guard only PUT/PATCH/DELETE)            → TestIssue287_CrossSitePlainPostIsRefused
//	S4 accept Origin "null"                                  → TestOriginGuard_RefusedOrigins (null)
//	S5 compare against the arrival port, not r.Host         → TestOriginGuard_AdmittedOrigins (proxy, vite)
//	S6 run the Origin check before the Host check           → TestOriginGuard_ReboundStill421
//	S7 guard OPTIONS                                         → TestOriginGuard_DesktopPreflight

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/config"
)

const originGuardFactBody = `{"title":"csrf","body":"written by a cross-site form","type":"observation","domain":["ai","ml"]}`

// originGuardServer is the full plain-listener Handler over a real loopback
// TCP listener, with a counting fact writer behind it.
func originGuardServer(t *testing.T, mutate func(*Server)) (*httptest.Server, *stubFactWriter) {
	t.Helper()
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.Defaults().Auth,
		OntologyRoot: "know", providers: storeProviders{factWriter: writer}}
	if mutate != nil {
		mutate(s)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, writer
}

type originReq struct {
	method, path, body, contentType string
	host                            string // "" = the listener's own 127.0.0.1:<port>
	origin                          string // "" = no Origin header
	extra                           http.Header
}

func (c originReq) do(t *testing.T, srv *httptest.Server) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	if c.body != "" {
		rd = strings.NewReader(c.body)
	}
	req, err := http.NewRequest(c.method, srv.URL+c.path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "" {
		req.Host = c.host
	}
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	for k, vs := range c.extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

func ownHostOf(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

const factsPath = "/api/v1/repos/alpha/branches/agent:test/facts"

// The request measured in the #281 RCA (reviewer input 5): a cross-site
// page's form post — text/plain, no preflight, a legitimate loopback Host —
// used to answer 201 and write a fact as anonymous@none.
func TestIssue287_CrossSitePlainPostIsRefused(t *testing.T) {
	srv, writer := originGuardServer(t, nil)
	code, body, _ := originReq{method: "POST", path: factsPath, body: originGuardFactBody,
		contentType: "text/plain", origin: "https://evil.example"}.do(t, srv)
	if code != http.StatusForbidden || !strings.Contains(body, "Cross-origin request refused") ||
		!strings.Contains(body, "evil.example") {
		t.Fatalf("cross-site text/plain POST: %d %.300s; want 403 Cross-origin request refused", code, body)
	}
	if writer.writeCalls != 0 {
		t.Fatalf("the cross-site POST reached the fact writer %d time(s)", writer.writeCalls)
	}

	// Control: the same request from the page this listener served is the
	// write it always was.
	code, body, _ = originReq{method: "POST", path: factsPath, body: originGuardFactBody,
		contentType: "application/json", origin: "http://" + ownHostOf(srv)}.do(t, srv)
	if code != http.StatusCreated || writer.writeCalls != 1 {
		t.Fatalf("same-origin POST: %d %.300s writeCalls=%d; want 201 and one write", code, body, writer.writeCalls)
	}
}

func TestOriginGuard_RefusedOrigins(t *testing.T) {
	srv, writer := originGuardServer(t, nil)
	own := ownHostOf(srv)
	for _, c := range []struct {
		name string
		req  originReq
	}{
		{"null (sandboxed iframe, file:, no-referrer form)", originReq{origin: "null"}},
		{"vite dev origin against the real listener", originReq{origin: "http://localhost:5173"}},
		{"same host, other port", originReq{origin: "http://127.0.0.1:1"}},
		{"own host, other scheme", originReq{origin: "ftp://" + own}},
		{"own host with a path", originReq{origin: "http://" + own + "/"}},
		{"own host with userinfo", originReq{origin: "http://x@" + own}},
		{"own host with a query", originReq{origin: "http://" + own + "?q"}},
		{"malformed", originReq{origin: "http://%zz"}},
		{"prefix of own host", originReq{origin: "http://" + own + ".evil.example"}},
		{"wails origin without the allowlist", originReq{origin: "wails://localhost"}},
		{"repeated Origin", originReq{extra: http.Header{"Origin": {"http://" + own, "http://" + own}}}},
	} {
		r := c.req
		r.method, r.path, r.body, r.contentType = "POST", factsPath, originGuardFactBody, "application/json"
		code, body, _ := r.do(t, srv)
		if code != http.StatusForbidden || !strings.Contains(body, "Cross-origin request refused") {
			t.Errorf("%s: %d %.200s; want 403 Cross-origin request refused", c.name, code, body)
		}
	}
	if writer.writeCalls != 0 {
		t.Fatalf("a refused origin reached the fact writer %d time(s)", writer.writeCalls)
	}
}

func TestOriginGuard_AdmittedOrigins(t *testing.T) {
	srv, writer := originGuardServer(t, func(s *Server) {
		s.Auth.LoopbackHosts = []string{"box.tail1234.ts.net"}
	})
	own := ownHostOf(srv)
	for _, c := range []struct {
		name string
		req  originReq
	}{
		{"no Origin (curl, CLI, bridge)", originReq{}},
		{"own origin", originReq{origin: "http://" + own}},
		{"own origin, other case", originReq{origin: "HTTP://" + strings.ToUpper(own)}},
		{"vite dev proxy: Host and Origin both localhost:5173", originReq{host: "localhost:5173", origin: "http://localhost:5173"}},
		{"https proxy for a listed name", originReq{host: "box.tail1234.ts.net", origin: "https://box.tail1234.ts.net"}},
		{"default port: Origin none, Host none", originReq{host: "localhost", origin: "http://localhost"}},
		{"default port: Origin none, Host :80", originReq{host: "localhost:80", origin: "http://localhost"}},
		{"default port: Origin :80, Host none", originReq{host: "localhost", origin: "http://localhost:80"}},
	} {
		r := c.req
		r.method, r.path, r.body, r.contentType = "POST", factsPath, originGuardFactBody, "application/json"
		before := writer.writeCalls
		code, body, _ := r.do(t, srv)
		if code != http.StatusCreated || writer.writeCalls != before+1 {
			t.Errorf("%s: %d %.200s; want 201 and a write", c.name, code, body)
		}
	}
}

// PUT, PATCH and DELETE are preflighted by any browser anyway, but the rule
// does not keep a method list that ages: every non-GET/HEAD/OPTIONS method is
// judged. Another plain-listener route is covered too, not only /facts.
func TestOriginGuard_EveryMutatingMethodAndRoute(t *testing.T) {
	srv, writer := originGuardServer(t, nil)
	for _, r := range []originReq{
		{method: "POST", path: "/api/v1/repos:probe-origin", body: `{"url":"https://example.com/x.git"}`, contentType: "text/plain"},
		{method: "PUT", path: "/api/v1/repos/alpha/branches/agent:test/facts/know/ai/x.md", body: `{"content":"` + testFactContent + `"}`, contentType: "application/json"},
		{method: "PATCH", path: "/api/v1/repos/alpha", body: `{"description":"pwned"}`, contentType: "application/json"},
		{method: "DELETE", path: "/api/v1/repos/alpha/branches/agent:test/facts/know/ai/x.md"},
	} {
		r.origin = "https://evil.example"
		code, body, _ := r.do(t, srv)
		if code != http.StatusForbidden || !strings.Contains(body, "Cross-origin request refused") {
			t.Errorf("%s %s: %d %.200s; want 403 Cross-origin request refused", r.method, r.path, code, body)
		}
	}
	if writer.writeCalls != 0 || writer.deleteCalls != 0 {
		t.Fatalf("a refused request reached the writer: writes=%d deletes=%d", writer.writeCalls, writer.deleteCalls)
	}

	// Reads are the Host check's business: a GET with a foreign Origin is
	// served as before.
	code, body, _ := originReq{method: "GET", path: "/api/v1/repos", origin: "https://evil.example"}.do(t, srv)
	if code != http.StatusOK {
		t.Fatalf("GET with a foreign Origin: %d %.200s; want 200", code, body)
	}
}

// The Host check runs first: a request that fails BOTH checks — a foreign
// Host and an Origin that is not that Host — gets the #281 421, never this
// 403. (A rebound page's Origin equals its own foreign Host, which passes the
// Origin rule, so it alone cannot tell the order apart.)
func TestOriginGuard_ReboundStill421(t *testing.T) {
	srv, _ := originGuardServer(t, nil)
	host := "attacker.example" + srv.URL[strings.LastIndex(srv.URL, ":"):]
	for _, origin := range []string{"http://" + host, "https://evil.example"} {
		code, body, _ := originReq{method: "POST", path: factsPath, body: originGuardFactBody,
			contentType: "text/plain", host: host, origin: origin}.do(t, srv)
		if code != http.StatusMisdirectedRequest {
			t.Fatalf("foreign Host, Origin %s: %d %.200s; want 421 from the Host check", origin, code, body)
		}
	}
}

var desktopOrigins = []string{"wails://localhost", "http://wails.localhost"}

// The desktop webview writes cross-origin from wails://localhost. AuthMiddleware
// runs on BOTH the outer router and the API router; a POST through the real
// Handler proves both carry the allowlist.
func TestOriginGuard_DesktopWailsOriginWrites(t *testing.T) {
	srv, writer := originGuardServer(t, func(s *Server) {
		s.APIOnly = true
		s.CORSOrigins = desktopOrigins
	})
	for _, origin := range desktopOrigins {
		before := writer.writeCalls
		code, body, hdr := originReq{method: "POST", path: factsPath, body: originGuardFactBody,
			contentType: "application/json", origin: origin}.do(t, srv)
		if code != http.StatusCreated || writer.writeCalls != before+1 || hdr.Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("desktop POST from %s: %d %.200s ACAO=%q; want 201, a write and ACAO", origin, code, body, hdr.Get("Access-Control-Allow-Origin"))
		}
	}
	// The allowlist is exact: a foreign origin is still refused on the desktop.
	code, body, _ := originReq{method: "POST", path: factsPath, body: originGuardFactBody,
		contentType: "text/plain", origin: "https://evil.example"}.do(t, srv)
	if code != http.StatusForbidden {
		t.Fatalf("desktop, foreign origin: %d %.200s; want 403", code, body)
	}
}

// OPTIONS is exempt: AuthMiddleware runs before corsMiddleware, and the
// desktop's own preflights must reach it.
func TestOriginGuard_DesktopPreflight(t *testing.T) {
	srv, _ := originGuardServer(t, func(s *Server) {
		s.APIOnly = true
		s.CORSOrigins = desktopOrigins
	})
	code, body, hdr := originReq{method: "OPTIONS", path: factsPath, origin: "wails://localhost",
		extra: http.Header{"Access-Control-Request-Method": {"POST"}, "Access-Control-Request-Headers": {"content-type"}}}.do(t, srv)
	if code != http.StatusNoContent || hdr.Get("Access-Control-Allow-Origin") != "wails://localhost" {
		t.Fatalf("desktop preflight: %d %.200s ACAO=%q; want 204 with ACAO", code, body, hdr.Get("Access-Control-Allow-Origin"))
	}
	// A foreign origin's preflight is refused by corsMiddleware withholding
	// Access-Control-Allow-Origin, exactly as before #287: the Origin guard
	// never answers an OPTIONS.
	code, body, hdr = originReq{method: "OPTIONS", path: factsPath, origin: "https://evil.example",
		extra: http.Header{"Access-Control-Request-Method": {"POST"}}}.do(t, srv)
	if code != http.StatusNoContent || hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign preflight: %d %.200s ACAO=%q; want corsMiddleware's 204 with no ACAO", code, body, hdr.Get("Access-Control-Allow-Origin"))
	}
}

func TestLoopbackOriginOK_Table(t *testing.T) {
	trusted := []string{"wails://localhost"}
	for _, c := range []struct {
		method, host string
		origins      []string // nil = no Origin header
		ok           bool
	}{
		{"GET", "127.0.0.1:19278", []string{"https://evil.example"}, true},
		{"HEAD", "127.0.0.1:19278", []string{"https://evil.example"}, true},
		{"OPTIONS", "127.0.0.1:19278", []string{"https://evil.example"}, true},
		{"POST", "127.0.0.1:19278", nil, true},
		{"POST", "127.0.0.1:19278", []string{"http://127.0.0.1:19278"}, true},
		{"POST", "[::1]:19278", []string{"http://[::1]:19278"}, true},
		{"POST", "[::1]", []string{"http://[::1]:80"}, true},
		{"POST", "[::1]:80", []string{"http://[::1]"}, true},
		{"POST", "localhost:443", []string{"https://localhost"}, true},
		{"POST", "localhost:443", []string{"http://localhost"}, false},
		{"POST", "[::1]:19278", []string{"http://[::1]:19279"}, false},
		{"POST", "127.0.0.1:19278", []string{"wails://localhost"}, true},
		{"POST", "127.0.0.1:19278", []string{"wails://LOCALHOST"}, false},
		{"POST", "127.0.0.1:19278", []string{"null"}, false},
		{"POST", "127.0.0.1:19278", []string{""}, false},
		{"POST", "", []string{"http://localhost"}, false},
		{"PUT", "127.0.0.1:19278", []string{"https://evil.example"}, false},
		{"PATCH", "127.0.0.1:19278", []string{"https://evil.example"}, false},
		{"DELETE", "127.0.0.1:19278", []string{"https://evil.example"}, false},
		{"PROPFIND", "127.0.0.1:19278", []string{"https://evil.example"}, false},
	} {
		r := httptest.NewRequest(c.method, "/x", nil)
		r.Host = c.host
		if c.origins != nil {
			r.Header["Origin"] = c.origins
		}
		if got := loopbackOriginOK(r, trusted); got != c.ok {
			t.Errorf("%s Host=%q Origin=%q: got %v, want %v", c.method, c.host, c.origins, got, c.ok)
		}
	}
}
