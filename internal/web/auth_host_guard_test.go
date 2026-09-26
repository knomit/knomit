package web

// #281: a loopback TCP request becomes the anonymous principal only when its
// Host names this machine the way a browser on it would: localhost, an IP
// literal, the bind host, or a name in [auth].loopback_hosts. Anything else
// is what a DNS-rebound page sends, and is refused with 421 before a
// principal exists.
//
// Sabotage rows (each was applied against a commit, one at a time, and made
// the named test fail):
//
//	S1 delete the guard call in AuthMiddleware        → TestIssue281_ReboundHostIsRefused
//	S2 compare the arrival port as well as the name   → TestLoopbackHostOK_Table (localhost:5173)
//	S3 run the guard before the socket peer branch    → TestHostGuard_SocketPeerIgnoresHost
//	S4 move the guard from AuthMiddleware to apiRouter → TestIssue281_ReboundHostIsRefused (/git, SPA)
//	                                                     and TestHostGuard_OAuthListenerIgnoresHost
//	S5 ignore loopback_hosts                           → TestHostGuard_ListedNameIsAdmitted
//	S6 prefix/substring matching of "localhost"        → TestLoopbackHostOK_Table (localhost.attacker.example)
//	S7 invert the IP-literal rule                      → TestLoopbackHostOK_Table
//	S8 drop the bind-host auto-include                 → internal/app TestApp_BindHostIsALoopbackHost
//	S9 compute the effective set at config.Load        → internal/app TestApp_BindHostIsALoopbackHost (--host case)

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// rebound sends what a DNS-rebound page's own fetch() puts on the wire, over
// a REAL loopback TCP connection to srv: Host and Origin name the attacker,
// and the page is same-origin with itself.
func rebound(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "attacker.example" + srv.URL[strings.LastIndex(srv.URL, ":"):]
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// The reproduction from the RCA (.claude/plans/2026-09-25-281-repro_test.go.txt),
// unchanged in shape: the full Handler() behind a real 127.0.0.1 listener,
// default [auth], and the rebound Host. Before the fix every line below was
// served as anonymous@none — the fact PUT answered 200 and reached the
// writer. Now every route, reads and the SPA included, is 421 and nothing
// reaches a handler.
func TestIssue281_ReboundHostIsRefused(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	git := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the git handler was reached")
	})
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.Defaults().Auth,
		providers: storeProviders{factWriter: writer}, GitHandler: git}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/repos", ""},
		{"GET", "/api/v1/repos/alpha", ""},
		{"PUT", "/api/v1/repos/alpha/branches/agent:test/facts/know/ai/pwn.md", `{"content":"` + testFactContent + `"}`},
		{"PATCH", "/api/v1/repos/alpha", `{"description":"pwned"}`},
		{"POST", "/api/v1/repos/alpha/rename", `{"name":"beta"}`},
		{"DELETE", "/api/v1/repos/alpha", ""},
		{"GET", "/api/v1/archived", ""},
		{"POST", "/api/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`},
		{"GET", "/git/alpha/info/refs?service=git-receive-pack", ""},
		{"GET", "/", ""},
		{"GET", "/docs", ""},
	} {
		code, body := rebound(t, srv, c.method, c.path, c.body)
		if code != http.StatusMisdirectedRequest || !strings.Contains(body, "Misdirected Request") ||
			!strings.Contains(body, "loopback_hosts") || !strings.Contains(body, "attacker.example") {
			t.Errorf("%s %s: got %d %.200s; want 421 naming the Host and [auth].loopback_hosts", c.method, c.path, code, body)
		}
	}
	if writer.writeCalls != 0 {
		t.Fatalf("the rebound PUT reached the fact writer %d time(s)", writer.writeCalls)
	}
}

// The same PUT with the server's own Host is served as before: the guard is
// the only thing standing between the two.
func TestIssue281_OwnHostStillWrites(t *testing.T) {
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.Defaults().Auth,
		providers: storeProviders{factWriter: writer}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("PUT", srv.URL+"/api/v1/repos/alpha/branches/agent:test/facts/know/ai/ok.md",
		strings.NewReader(`{"content":"`+testFactContent+`"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || writer.writeCalls != 1 {
		t.Fatalf("own Host %s: %d, writeCalls=%d", req.URL.Host, res.StatusCode, writer.writeCalls)
	}
}

func TestLoopbackHostOK_Table(t *testing.T) {
	listed := []string{"box.tail1234.ts.net"}
	for _, c := range []struct {
		host string
		ok   bool
	}{
		// Loopback spellings, with and without a port.
		{"localhost", true},
		{"localhost:19278", true},
		{"LOCALHOST:19278", true},
		{"127.0.0.1:19278", true},
		{"[::1]:19278", true},
		{"[::1]", true},
		// The name, never the port: vite's dev proxy forwards Host localhost:5173.
		{"localhost:5173", true},
		// Any IP literal: DNS rebinding needs a name.
		{"127.0.0.2:19278", true},
		{"100.64.0.1:8080", true},
		{"192.168.1.5", true},
		{"[fe80::1]:19278", true},
		// HTTP/1.0 with no Host: no browser sends it.
		{"", true},
		// A listed name, compared case-insensitively.
		{"box.tail1234.ts.net", true},
		{"BOX.tail1234.ts.net:443", true},
		// Everything else is a name someone else may answer DNS for.
		{"attacker.example", false},
		{"attacker.example:19278", false},
		{"localhost.attacker.example:19278", false},
		{"attacker.localhost:19278", false},
		{"app.localhost", false},
		{"127.0.0.1.nip.io:19278", false},
		{"localhost.:19278", false},
		{"box2.tail1234.ts.net", false},
		{"tail1234.ts.net", false},
	} {
		if got := loopbackHostOK(c.host, listed); got != c.ok {
			t.Errorf("loopbackHostOK(%q) = %v, want %v", c.host, got, c.ok)
		}
	}
}

// An absent Host header (HTTP/1.0), sent as raw bytes: net/http leaves
// r.Host empty, and the request is the anonymous principal as before.
func TestHostGuard_AbsentHostHTTP10IsAdmitted(t *testing.T) {
	srv := httptest.NewServer(AuthMiddleware(config.AuthConfig{}, false)(principalEcho()))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /x HTTP/1.0\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(b) != "anonymous@none" {
		t.Fatalf("HTTP/1.0 without Host: %d %q", res.StatusCode, b)
	}
}

func hostReq(remote, host string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = remote
	r.Host = host
	return r
}

func TestHostGuard_ListedNameIsAdmitted(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{LoopbackHosts: []string{"box.tail1234.ts.net"}}, false)(principalEcho())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, hostReq("127.0.0.1:5555", "BOX.tail1234.ts.net:443"))
	if rr.Code != http.StatusOK || rr.Body.String() != "anonymous@none" {
		t.Fatalf("listed name: %d %q", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, hostReq("127.0.0.1:5555", "box2.tail1234.ts.net"))
	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unlisted name: %d %q", rr.Code, rr.Body.String())
	}
}

// The socket/pipe listener: `knomit oauth` sends Host: knomit.local
// (cmd/oauth.go), and the kernel, not the Host, says who is calling.
func TestHostGuard_SocketPeerIgnoresHost(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{}, false)(principalEcho())
	req := hostReq("@", "knomit.local")
	req = req.WithContext(auth.WithPeer(req.Context(), testPeer(501, 7)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "bridge:uid:501@socket" {
		t.Fatalf("socket peer with Host knomit.local: %d %q", rr.Code, rr.Body.String())
	}
}

// The TLS listener never mints anonymous: a foreign Host there meets the
// certificate refusal, exactly as before.
func TestHostGuard_TLSListenerIgnoresHost(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{}, false)(principalEcho())
	req := hostReq("127.0.0.1:5555", "attacker.example")
	req.TLS = &tls.ConnectionState{}
	req = req.WithContext(auth.TLSConnContext(req.Context(), nil))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "Authentication required") {
		t.Fatalf("TLS listener: %d %q", rr.Code, rr.Body.String())
	}
}

// A non-loopback peer is untouched: no principal, reads served, writes
// refused by the write gate — the Host it sends changes nothing it can do.
func TestHostGuard_NonLoopbackPeerIgnoresHost(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.Defaults().Auth}
	h := s.Handler()
	get := hostReq("192.0.2.1:1234", "mybox.lan:19278")
	get.URL.Path = "/api/v1/repos"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, get)
	if rr.Code != http.StatusOK {
		t.Fatalf("LAN GET: %d %s", rr.Code, rr.Body.String())
	}
	patch := newJSONRequest("PATCH", "/api/v1/repos/alpha", strings.NewReader(`{}`))
	patch.Host = "mybox.lan:19278"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, patch)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "Permission denied") {
		t.Fatalf("LAN PATCH: %d %s", rr.Code, rr.Body.String())
	}
}

// require = true: the anonymous branch is never entered, so the existing
// "Authentication required" refusal is what a rebound request meets.
func TestHostGuard_RequireTrueKeepsItsRefusal(t *testing.T) {
	h := AuthMiddleware(config.AuthConfig{Require: true}, false)(principalEcho())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, hostReq("127.0.0.1:5555", "attacker.example"))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "Authentication required") {
		t.Fatalf("require=true: %d %q", rr.Code, rr.Body.String())
	}
}

// The OAuth listener never runs AuthMiddleware: a same-host TLS proxy
// delivers the public issuer host from loopback, and a bearer is served.
func TestHostGuard_OAuthListenerIgnoresHost(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	tok := f.mint(t, "laptop", testIssuer, []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	req := fromLoopback(httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	req.Host = "knomit.example.com"
	rec := serve(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OAuth listener behind a same-host proxy: %d %s", rec.Code, rec.Body.String())
	}
}

// The desktop: the webview (origin wails://localhost) calls
// http://127.0.0.1:<port>, preflight included.
func TestHostGuard_DesktopWebviewShapeIsAdmitted(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha"), Auth: config.Defaults().Auth,
		APIOnly: true, CORSOrigins: []string{"wails://localhost", "http://wails.localhost"}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, method := range []string{http.MethodOptions, http.MethodGet} {
		req, _ := http.NewRequest(method, srv.URL+"/api/v1/repos", nil)
		req.Header.Set("Origin", "wails://localhost")
		req.Header.Set("Access-Control-Request-Method", "GET")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode >= 400 || res.Header.Get("Access-Control-Allow-Origin") != "wails://localhost" {
			t.Fatalf("%s from the webview: %d ACAO=%q", method, res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
		}
	}
}
