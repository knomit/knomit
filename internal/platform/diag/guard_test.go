package diag

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

// TestGuard_Table pins the three refusals (#288) and their order across a
// read route and a mutating one.
func TestGuard_Table(t *testing.T) {
	s := NewServer(Options{LoopbackHosts: []string{"box.tail1234.ts.net"}})
	for _, c := range []struct {
		name    string
		method  string
		target  string
		remote  string
		host    string
		headers map[string]string
		want    int
	}{
		{"curl by IP", "GET", "/runtime/status", "127.0.0.1:1", "127.0.0.1:6060", nil, http.StatusOK},
		{"curl by localhost", "GET", "/metrics", "[::1]:1", "localhost:6060", nil, http.StatusOK},
		{"listed name", "GET", "/runtime/status", "127.0.0.1:1", "BOX.tail1234.ts.net:6060", nil, http.StatusOK},
		{"HTTP/1.0 no Host", "GET", "/runtime/status", "127.0.0.1:1", "", nil, http.StatusOK},
		{"address bar", "GET", "/runtime/status", "127.0.0.1:1", "localhost:6060",
			map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"same-origin fetch", "GET", "/runtime/status", "127.0.0.1:1", "localhost:6060",
			map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},

		{"rebound DNS Host", "GET", "/debug/pprof/cmdline", "127.0.0.1:1", "attacker.example:6060",
			map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusMisdirectedRequest},
		{"rebound DNS Host, POST", "POST", "/runtime/gc", "127.0.0.1:1", "attacker.example:6060", nil, http.StatusMisdirectedRequest},
		{"Origin with IP Host", "POST", "/runtime/gc", "127.0.0.1:1", "127.0.0.1:6060",
			map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"Origin null", "GET", "/runtime/status", "127.0.0.1:1", "127.0.0.1:6060",
			map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"cross-site", "GET", "/debug/pprof/profile?seconds=1", "127.0.0.1:1", "127.0.0.1:6060",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site", "GET", "/runtime/status", "127.0.0.1:1", "localhost:6060",
			map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"non-loopback peer", "GET", "/metrics", "10.0.0.7:1", "127.0.0.1:6060", nil, http.StatusForbidden},
		{"httptest default peer", "GET", "/metrics", "192.0.2.1:1234", "localhost", nil, http.StatusForbidden},
		// Order: a non-loopback peer is refused as such even with a bad Host.
		{"non-loopback peer, rebound Host", "GET", "/metrics", "10.0.0.7:1", "attacker.example", nil, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.target, nil)
			req.RemoteAddr = c.remote
			req.Host = c.host
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Fatalf("status = %d, want %d; body %q", rr.Code, c.want, rr.Body.String())
			}
		})
	}
}

// TestGuard_NoCorsPostChangesNothing is the attack the Host check alone
// misses: a page's fetch(..., {method: "POST", mode: "no-cors"}) to
// http://127.0.0.1:<port> — a simple request, sent without preflight, from a
// loopback peer, with an IP-literal Host. It must not reach the handler.
func TestGuard_NoCorsPostChangesNothing(t *testing.T) {
	prev := zerolog.GlobalLevel()
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	req := httptest.NewRequest("POST", "/runtime/loglevel?level=trace", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Host = "127.0.0.1:6060"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "no-cors")
	rr := httptest.NewRecorder()
	NewServer(Options{}).Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if got := zerolog.GlobalLevel(); got != zerolog.InfoLevel {
		t.Fatalf("log level changed to %s by a cross-site request", got)
	}
}

// With AllowRemotePeers ([runtime].allow_remote) a remote scraper is served,
// but the browser and Host checks still apply to it.
func TestGuard_AllowRemotePeers(t *testing.T) {
	s := NewServer(Options{AllowRemotePeers: true})
	for _, c := range []struct {
		name   string
		host   string
		origin string
		want   int
	}{
		{"remote scraper by IP", "10.0.0.2:6060", "", http.StatusOK},
		{"remote, DNS Host", "knomit.internal:6060", "", http.StatusMisdirectedRequest},
		{"remote, browser", "10.0.0.2:6060", "http://evil.example", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/metrics", nil)
			req.RemoteAddr = "10.0.0.7:1"
			req.Host = c.host
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != c.want {
				t.Fatalf("status = %d, want %d; body %q", rr.Code, c.want, rr.Body.String())
			}
		})
	}
}
