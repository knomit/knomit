//go:build desktop

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/foundation/auth"
)

// The webview reaches the knomit API through Wails' asset handler, never
// through a TCP port. Two paths carry it, both dialing the server's local
// socket / named pipe with auth.DialLocal (the dialer kb uses, so the server
// sees the same verified local peer):
//
//   - request/response calls: apiProxy, a reverse proxy mounted on /api/ in the
//     asset handler (this file);
//   - the event streams: the "knomit-sse" Wails Stream (uistreams.go), because
//     Wails' Windows response writer buffers a response until the handler
//     returns and so cannot carry a long-lived SSE body.
//
// Nothing under tools/desktop may dial TCP for the UI path; the archtest
// TestDesktopUIPathNeverDialsTCP pins that.

// apiPrefix is the only path space the asset handler proxies upstream.
const apiPrefix = "/api/"

// desktopOrigin is the origin Wails serves the UI from on this platform
// (darwin and linux use the wails scheme, windows http). It is what
// /config.js and /boot/status report as the API base: every API call is
// same-origin, so the base is the page's own origin and no CORS applies.
func desktopOrigin() string {
	if runtime.GOOS == "windows" {
		return "http://wails.localhost"
	}
	return "wails://localhost"
}

// uiUpstream is where the webview's API traffic goes: the server's local
// socket / pipe path. It is empty until the supervisor has seen a knomit
// answer GET /api/v1/version there (the server it spawned, or one it attached
// to), so the UI is never pointed at a path whose holder is not a knomit.
type uiUpstream struct{ path atomic.Value } // string

func (u *uiUpstream) set(p string) { u.path.Store(p) }

func (u *uiUpstream) get() string {
	p, _ := u.path.Load().(string)
	return p
}

var errNoUpstream = errors.New("the knomit server is not listening on its local socket yet")

// dialTimeout bounds one dial of the local socket / pipe.
const dialTimeout = 5 * time.Second

// newUpstreamTransport builds the transport every UI-path request uses: a dial
// to the local socket / pipe and nothing else. Compression is off because a
// compressing hop is what stalls SSE (kb gotcha a762359e) and the hop is a local
// socket, where it buys nothing. keepAlive false gives each request its own
// connection, which the stream adapter wants so that cancelling a subscription
// closes exactly its own connection.
func newUpstreamTransport(up *uiUpstream, keepAlive bool) *http.Transport {
	return &http.Transport{
		DisableCompression:  true,
		DisableKeepAlives:   !keepAlive,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			p := up.get()
			if p == "" {
				return nil, errNoUpstream
			}
			return auth.DialLocal(ctx, p, dialTimeout)
		},
	}
}

// problem is the error body the UI already understands: api.ts reads
// detail, then title, then error (errorText), the shape the server's own
// problem+json answers have.
type problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{Title: http.StatusText(status), Status: status, Detail: detail})
}

// apiProxy mounts a reverse proxy for /api/ in front of next (the asset
// handler); everything else, /config.js and /boot/status included, goes to
// next. ready reports whether the server has finished booting.
//
// Booting: /api/ answers 503 with Retry-After rather than 502. The UI's boot
// gate (App.tsx serverBooting) keeps its own calls back until /boot/status says
// ready, so only an ungated poller reaches this window; the one known is
// useRepoCreates' first poll, whose catch keeps the last known list and retries
// on its own timer. Holding the request until ready was rejected: a first launch
// spends minutes downloading models, and a request parked that long ties up a
// Wails request goroutine for nothing the UI can use.
//
// The request context is the Wails request lifetime (cancelled when the webview
// aborts the request, on every platform since beta.20/23), and ReverseProxy
// ties the upstream request to it, so a fetch abort closes the upstream
// connection.
func apiProxy(next http.Handler, ready func() bool, up *uiUpstream) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "localhost"
			pr.Out.Host = "localhost"
			// The server's origin checks apply to TCP peers; a socket peer
			// skips them, and the wails:// origin means nothing there.
			pr.Out.Header.Del("Origin")
			for k := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(k), "x-wails-") {
					pr.Out.Header.Del(k)
				}
			}
		},
		Transport: newUpstreamTransport(up, true),
		// Flush every write: SSE and the streamed create/apply responses
		// must not wait for a buffer to fill.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the webview went away; nobody reads the answer
			}
			log.Warn().Err(err).Str("path", r.URL.Path).Msg("desktop api proxy: upstream failed")
			writeProblem(w, http.StatusBadGateway, "could not reach the knomit server")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if !ready() {
			writeProblem(w, http.StatusServiceUnavailable, "knomit is still starting")
			return
		}
		// Wails derives ContentLength only from a Content-Length header, which
		// a WKWebView custom-scheme POST does not carry, so a request with a
		// body arrives as ContentLength 0 — and ReverseProxy sends no body at
		// all when it is 0. -1 means "unknown, chunked".
		if r.ContentLength == 0 && r.Body != nil && r.Body != http.NoBody {
			r.ContentLength = -1
		}
		rp.ServeHTTP(w, r)
	})
}
