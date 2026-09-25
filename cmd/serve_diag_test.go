package cmd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"knomit/internal/config"
)

// Off unless [runtime].addr is set: no listener, no error.
func TestOpenDiagServer_OffByDefault(t *testing.T) {
	srv, ln, err := openDiagServer(config.Defaults(), &http.Server{}, nil)
	if srv != nil || ln != nil || err != nil {
		t.Fatalf("[runtime] unset: %v %v %v", srv, ln, err)
	}
}

// A taken port is an error returned before "knomit ready" (#288), not a
// warning from a goroutine afterwards.
func TestOpenDiagServer_BindFailureIsAnError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg := config.Defaults()
	cfg.Runtime.Addr = busy.Addr().String()
	if _, _, err := openDiagServer(cfg, &http.Server{}, nil); err == nil || !strings.Contains(err.Error(), "runtime diagnostics") {
		t.Fatalf("binding a taken address: want a runtime diagnostics error, got %v", err)
	}
}

// The wiring, over a real loopback listener: the guard is in front, the bind
// host is an admitted Host name (from cfg.Host through
// EffectiveLoopbackHosts), and like's timeouts and base context are copied.
func TestOpenDiagServer_GuardIsWired(t *testing.T) {
	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.Host = "MyBox.example"
	cfg.Runtime.Addr = "127.0.0.1:0"
	like := &http.Server{
		ReadHeaderTimeout: 7 * time.Second,
		ReadTimeout:       11 * time.Second,
		IdleTimeout:       13 * time.Second,
		WriteTimeout:      17 * time.Second, // must NOT be copied
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	srv, ln, err := openDiagServer(cfg, like, nil)
	if err != nil || srv == nil || ln == nil {
		t.Fatalf("open: %v %v %v", srv, ln, err)
	}
	if srv.ReadHeaderTimeout != 7*time.Second || srv.ReadTimeout != 11*time.Second ||
		srv.IdleTimeout != 13*time.Second || srv.BaseContext == nil {
		t.Fatalf("timeouts/base context not copied: header=%v read=%v idle=%v base=%v",
			srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout, srv.BaseContext != nil)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, want 0: a limit would cut off large heap and goroutine dumps", srv.WriteTimeout)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	for _, c := range []struct {
		name, method, host, origin string
		want                       int
	}{
		{"plain curl", "GET", "", "", http.StatusOK},
		{"bind host name", "GET", "mybox.example", "", http.StatusOK},
		{"rebound Host", "GET", "attacker.example", "", http.StatusMisdirectedRequest},
		{"browser POST", "POST", "", "http://evil.example", http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, "http://"+ln.Addr().String()+"/runtime/gc", nil)
			if err != nil {
				t.Fatal(err)
			}
			if c.method == "GET" {
				req.URL.Path = "/runtime/status"
			}
			if c.host != "" {
				req.Host = net.JoinHostPort(c.host, strconv.Itoa(port))
			}
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

// [runtime].allow_remote reaches the guard: without it a non-loopback peer is
// refused, with it the peer is served.
func TestOpenDiagServer_AllowRemoteIsWired(t *testing.T) {
	for _, allow := range []bool{false, true} {
		cfg := config.Defaults()
		cfg.Home = t.TempDir()
		cfg.Runtime.Addr = "127.0.0.1:0"
		cfg.Runtime.AllowRemote = allow
		srv, ln, err := openDiagServer(cfg, &http.Server{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ln.Close()
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.RemoteAddr = "10.0.0.7:1"
		req.Host = "10.0.0.2:6060"
		rr := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rr, req)
		want := http.StatusForbidden
		if allow {
			want = http.StatusOK
		}
		if rr.Code != want {
			t.Errorf("allow_remote=%v: status = %d, want %d", allow, rr.Code, want)
		}
	}
}
