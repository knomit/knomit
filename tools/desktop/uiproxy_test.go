//go:build desktop

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// startLocalUpstream serves h on a unix socket in a short temp dir (unix socket
// paths are capped near 100 bytes, which t.TempDir() overruns on macOS) and
// returns an uiUpstream pointing at it. It stands in for the server's local
// listener; the proxy and the stream adapter dial it through auth.DialLocal.
func startLocalUpstream(t *testing.T, h http.Handler) *uiUpstream {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the local listener is a named pipe on windows")
	}
	dir, err := os.MkdirTemp("", "kui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	up := &uiUpstream{}
	up.set(sock)
	return up
}

func always(v bool) func() bool { return func() bool { return v } }

// assetStub stands in for the asset handler behind the proxy.
var assetStub = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "asset:"+r.URL.Path)
})

func TestAPIProxy_BodyWithoutLengthReachesTheServer(t *testing.T) {
	var gotBody, gotTE, gotCL string
	up := startLocalUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotTE, gotCL = string(b), strings.Join(r.TransferEncoding, ","), r.Header.Get("Content-Length")
		w.WriteHeader(http.StatusCreated)
	}))
	h := apiProxy(assetStub, always(true), up)

	// What Wails hands the handler for a webview POST: a body and
	// ContentLength 0, because no Content-Length header was sent.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos", io.NopCloser(strings.NewReader(`{"name":"a"}`)))
	req.ContentLength = 0
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	if gotBody != `{"name":"a"}` {
		t.Fatalf("upstream body = %q: the POST body was dropped", gotBody)
	}
	// Positive control: the length really was unknown, so this exercised the
	// -1 path and not a fixed-length one.
	if gotCL != "" || gotTE != "chunked" {
		t.Fatalf("expected a chunked upstream request, got Content-Length %q Transfer-Encoding %q", gotCL, gotTE)
	}
}

func TestAPIProxy_StripsWailsAndOriginHeadersAndRewritesHost(t *testing.T) {
	var got http.Header
	var gotHost, gotPath, gotQuery string
	up := startLocalUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotHost, gotPath, gotQuery = r.Header.Clone(), r.Host, r.URL.Path, r.URL.RawQuery
	}))
	h := apiProxy(assetStub, always(true), up)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos?x=1", nil)
	req.Host = "localhost"
	req.Header.Set("Origin", "wails://localhost")
	req.Header.Set("x-wails-window-id", "1")
	req.Header.Set("X-Wails-Window-Name", "main")
	req.Header.Set("X-Knomit-Client", "web")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got == nil {
		t.Fatal("the upstream was never reached")
	}
	for _, k := range []string{"Origin", "X-Wails-Window-Id", "X-Wails-Window-Name"} {
		if v := got.Get(k); v != "" {
			t.Errorf("header %s = %q reached the server", k, v)
		}
	}
	if got.Get("X-Knomit-Client") != "web" {
		t.Errorf("an ordinary header was lost: %v", got)
	}
	if gotHost != "localhost" || gotPath != "/api/v1/repos" || gotQuery != "x=1" {
		t.Errorf("host/path/query = %q %q %q", gotHost, gotPath, gotQuery)
	}
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body %q is not the problem JSON the UI reads: %v", rec.Body.String(), err)
	}
	return p
}

func TestAPIProxy_BootingAnswers503WithRetryAfterAndDoesNotDial(t *testing.T) {
	var hits atomic.Int32
	up := startLocalUpstream(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	h := apiProxy(assetStub, always(false), up)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repo-creates", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on the booting answer")
	}
	if p := decodeProblem(t, rec); p.Status != 503 || p.Detail == "" {
		t.Errorf("problem = %+v", p)
	}
	if hits.Load() != 0 {
		t.Error("the booting answer still reached the server")
	}
}

func TestAPIProxy_UnreachableUpstreamIs502WithAProblemBody(t *testing.T) {
	for name, path := range map[string]string{"no socket recorded": "", "nothing listens there": "/nonexistent/knomit.sock"} {
		t.Run(name, func(t *testing.T) {
			up := &uiUpstream{}
			if path != "" {
				up.set(path)
			}
			rec := httptest.NewRecorder()
			apiProxy(assetStub, always(true), up).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", rec.Code)
			}
			p := decodeProblem(t, rec)
			if p.Status != 502 || p.Detail == "" {
				t.Errorf("problem = %+v", p)
			}
			if strings.Contains(rec.Body.String(), "nonexistent") {
				t.Errorf("the 502 body leaks the socket path: %s", rec.Body.String())
			}
			if rec.Header().Get("Retry-After") != "" {
				t.Error("502 must not carry the booting Retry-After")
			}
		})
	}
}

func TestAPIProxy_OnlyAPIPathsAreProxied(t *testing.T) {
	var hits atomic.Int32
	up := startLocalUpstream(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	h := apiProxy(assetStub, always(true), up)
	for _, p := range []string{"/config.js", "/boot/status", "/", "/apix", "/desktop/settings.html"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Body.String() != "asset:"+p {
			t.Errorf("%s went to %q, want the asset handler", p, rec.Body.String())
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d non-API requests reached the server", hits.Load())
	}
}

// An SSE response must reach the client event by event while the upstream is
// still open, and the client going away must close the upstream.
func TestAPIProxy_StreamsEachEventAndCancelClosesTheUpstream(t *testing.T) {
	release := make(chan struct{})
	upstreamDone := make(chan error, 1)
	up := startLocalUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: ready\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "event: index\ndata: 1\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
		<-r.Context().Done()
		upstreamDone <- r.Context().Err()
	}))
	front := httptest.NewServer(apiProxy(assetStub, always(true), up))
	t.Cleanup(front.Close)

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/api/v1/repo-events", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)

	line, err := br.ReadString('\n')
	if err != nil || line != "event: ready\n" {
		t.Fatalf("first line = %q, %v: the event was not flushed through while the upstream is still open", line, err)
	}
	close(release)
	var sawIndex bool
	for i := 0; i < 4 && !sawIndex; i++ {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		sawIndex = l == "event: index\n"
	}
	if !sawIndex {
		t.Fatal("the second event never arrived")
	}

	cancel() // the webview aborts the request
	select {
	case err := <-upstreamDone:
		if err == nil {
			t.Fatal("upstream context ended without an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelling the client did not close the upstream connection")
	}
}
