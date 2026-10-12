//go:build desktop

package main

// SPIKE (spike/wails-asset-transport): reverse-proxy /api/* from the Wails
// asset handler onto the server's local socket. Not production code; gated by
// KNOMIT_SPIKE_PROXY=1 so the default desktop is unchanged.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"knomit/internal/auth"
	"knomit/internal/config"
)

//go:embed spike_test.html
var spikeTestPage []byte

func spikeEnabled() bool { return os.Getenv("KNOMIT_SPIKE_PROXY") == "1" }

// spikeOrigin is the page origin Wails serves from on this platform.
func spikeOrigin() string {
	if runtime.GOOS == "windows" {
		return "http://wails.localhost"
	}
	return "wails://localhost"
}

var (
	spikeOpenConns  atomic.Int64
	spikeTotalConns atomic.Int64
	spikeLogMu      sync.Mutex
)

func spikeLogf(format string, a ...any) {
	spikeLogMu.Lock()
	defer spikeLogMu.Unlock()
	fmt.Fprintf(os.Stderr, "SPIKE %s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}

type spikeConn struct {
	net.Conn
	once sync.Once
}

func (c *spikeConn) Close() error {
	c.once.Do(func() { spikeOpenConns.Add(-1); spikeLogf("upstream conn closed open=%d", spikeOpenConns.Load()) })
	return c.Conn.Close()
}

// timingWriter logs every Write to an SSE response and passes Flush through.
type timingWriter struct {
	http.ResponseWriter
	path string
	n    int
}

func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
func (t *timingWriter) Write(b []byte) (int, error) {
	t.n++
	if strings.Contains(t.Header().Get("Content-Type"), "text/event-stream") {
		first := strings.SplitN(string(b), "\n", 2)[0]
		spikeLogf("handler write #%d %s bytes=%d first=%q", t.n, t.path, len(b), first)
	}
	return t.ResponseWriter.Write(b)
}

func spikeAPIProxy(next http.Handler) http.Handler {
	sock, err := config.SocketPath()
	if err != nil {
		spikeLogf("no socket path: %v", err)
		return next
	}
	tr := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := auth.DialLocal(ctx, sock, 5*time.Second)
			if err != nil {
				return nil, err
			}
			spikeOpenConns.Add(1)
			spikeTotalConns.Add(1)
			spikeLogf("upstream conn opened open=%d", spikeOpenConns.Load())
			return &spikeConn{Conn: c}, nil
		},
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "localhost"
			pr.Out.Host = "localhost"
			pr.Out.Header.Del("Origin")
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			spikeLogf("proxy error %s: %v", r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
	}
	_ = url.URL{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/spike/test.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(spikeTestPage)
		case r.URL.Path == "/spike/sleep":
			ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
			spikeLogf("sleep %dms begin", ms)
			time.Sleep(time.Duration(ms) * time.Millisecond)
			spikeLogf("sleep end")
			_, _ = w.Write([]byte("ok"))
		case r.URL.Path == "/spike/stats":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int64{"open": spikeOpenConns.Load(), "total": spikeTotalConns.Load()})
		case r.URL.Path == "/spike/log":
			b, _ := io.ReadAll(r.Body)
			spikeLogf("PAGE %s", b)
		case strings.HasPrefix(r.URL.Path, "/api/"):
			start := time.Now()
			spikeLogf("proxy begin %s %s", r.Method, r.URL.Path)
			defer func() {
				spikeLogf("proxy end %s after %s ctxErr=%v", r.URL.Path, time.Since(start).Round(time.Millisecond), r.Context().Err())
			}()
			// Finding: Wails sets ContentLength only from a Content-Length header,
			// which a WKWebView custom-scheme POST does not carry; ReverseProxy
			// nils the body when ContentLength == 0. Declare it unknown.
			if r.ContentLength == 0 && r.Body != nil && r.Body != http.NoBody {
				spikeLogf("body present with ContentLength 0 on %s %s: forcing -1", r.Method, r.URL.Path)
				r.ContentLength = -1
			}
			rp.ServeHTTP(&timingWriter{ResponseWriter: w, path: r.URL.Path}, r)
		default:
			spikeLogf("asset %s %s", r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
		}
	})
}

// spikeWrap is the asset handler: the stock one, plus (when enabled) the
// /api/ proxy and a same-origin API base.
func spikeWrap(uiFS, desktopFS fs.FS, status func() bootStatus) http.Handler {
	if !spikeEnabled() {
		return configInjectingHandler(uiFS, desktopFS, status)
	}
	st := func() bootStatus {
		s := status()
		if s.Ready {
			s.APIBase = spikeOrigin()
		}
		return s
	}
	return spikeAPIProxy(configInjectingHandler(uiFS, desktopFS, st))
}

func spikeStartURL() string {
	if spikeEnabled() && os.Getenv("KNOMIT_SPIKE_PAGE") == "1" {
		return "/spike/test.html"
	}
	return "/"
}
