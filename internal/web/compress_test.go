package web

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/platform/logging"
	"knomit/internal/repos"
)

// gunzip reads a gzip stream whole, failing the test on any error. Every
// compression assertion below ends here: a Content-Encoding header is a claim,
// and the only thing that checks the claim is decoding the body.
func gunzip(t *testing.T, r io.Reader) []byte {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return b
}

// The API has always claimed to be compressed and never was: chi's default
// allowlist carries application/json but not application/hal+json, which is
// what every knomit API body actually is. See
// kb/gotchas/web/http/chi-compress-allowlist/72631d2b.md.
func TestAPI_HALResponsesAreGzipped(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	plain := httptest.NewRecorder()
	r.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/version", nil))
	if plain.Code != http.StatusOK {
		t.Fatalf("plain status: got %d, want 200", plain.Code)
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("plain Content-Encoding: got %q, want empty", got)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/hal+json" {
		t.Fatalf("content-type: got %q, want application/hal+json", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary: got %q, want it to contain Accept-Encoding", got)
	}

	body := gunzip(t, rec.Body)
	if string(body) != plain.Body.String() {
		t.Errorf("gunzipped body differs from the plain body:\n gzip:  %s\n plain: %s", body, plain.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("gunzipped body is not JSON: %v", err)
	}
	if _, ok := parsed["full"]; !ok {
		t.Errorf("version body has no %q key: %s", "full", body)
	}
}

// problem+json is the other content type the default allowlist misses, and
// it is what every error response carries.
func TestAPI_ProblemResponsesAreGzipped(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("content-type: got %q, want application/problem+json", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}

	var parsed map[string]any
	if err := json.Unmarshal(gunzip(t, rec.Body), &parsed); err != nil {
		t.Fatalf("gunzipped problem body is not JSON: %v", err)
	}
	if parsed["title"] != "Repo not found" {
		t.Errorf("problem title: got %v, want %q", parsed["title"], "Repo not found")
	}
}

// The allowlist must never grow a text/* wildcard or text/event-stream. A
// compressor buffers, and a buffered SSE stream is a stream that never
// arrives — see kb/gotchas/web/sse/proxy-compression-stalls-streams.
func TestSSE_IsNeverCompressed(t *testing.T) {
	s, _ := branchEventsServer(t)
	srv := httptest.NewServer(s.NewAPIRouter())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/repos/alpha/branches/agent:test/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Setting it by hand also turns off the transport's own transparent
	// gzip, so what arrives here is exactly what the server sent.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type: got %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE was compressed: Content-Encoding %q, want empty", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-transform") {
		t.Errorf("Cache-Control: got %q, want it to contain no-transform", got)
	}

	// Headers alone could be a lie about a buffered body, so read a frame.
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream ended before the initial status frame")
			}
			if line == "event: status" {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the initial status frame")
		}
	}
}

// The API router carries its own compressor and the outer router now carries
// one for the static routes. A request that passed through both would be
// gzipped twice and unreadable by every browser.
func TestServerHandler_APIIsCompressedExactlyOnce(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, APIBase+"/version", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}
	// One pass of gunzip must yield JSON. Two compressors would leave a
	// second gzip stream here instead.
	var parsed map[string]any
	if err := json.Unmarshal(gunzip(t, rec.Body), &parsed); err != nil {
		t.Fatalf("body is not JSON after a single gunzip — compressed twice? %v", err)
	}
}

// openapi.yaml is 173,770 bytes and sits inside the compressed router, so a
// content type missing from the allowlist costs ~137 KB on every fetch.
func TestAPI_OpenAPISpecIsGzipped(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/yaml" {
		t.Fatalf("content-type: got %q, want application/yaml", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}
	body := gunzip(t, rec.Body)
	if string(body) != string(openapiSpec) {
		t.Errorf("gunzipped spec differs from the embedded one (%d vs %d bytes)", len(body), len(openapiSpec))
	}
	if rec.Body.Len() >= len(openapiSpec) {
		t.Errorf("compressed size %d is not smaller than the %d-byte source", rec.Body.Len(), len(openapiSpec))
	}
}

// The ontologies endpoint writes "text/yaml; charset=utf-8". chi cuts the
// media type at ";" before matching, so the bare "text/yaml" entry has to be
// the thing that matches — this pins that, without needing a live ontology.
func TestCompressor_MatchesYAMLWithCharsetParameter(t *testing.T) {
	for _, ct := range []string{"text/yaml; charset=utf-8", "application/yaml", "application/hal+json"} {
		t.Run(ct, func(t *testing.T) {
			h := compressor()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = w.Write([]byte(strings.Repeat("compress me please\n", 100)))
			}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			h.ServeHTTP(rec, req)

			if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding for %q: got %q, want gzip", ct, got)
			}
			if got := len(gunzip(t, rec.Body)); got != 1900 {
				t.Errorf("gunzipped length: got %d, want 1900", got)
			}
		})
	}
}

// Server.Handler() is what the deployed binary runs; NewAPIRouter() alone is
// not, so the SSE guarantee is checked through the full chain.
//
// ONE STREAM PER HANDLER FAMILY — not every stream the server exposes. There
// are ten SSE entry points; the four below reach handlers_client_sessions.go,
// handlers_events.go, handlers_repo_events.go and handlers_logs.go. The job
// streams (handlers_jobs.go, two routes) and the four origin-session streams
// (handlers_origin_session.go) are NOT exercised here, because each needs a
// live job or origin session to produce a frame.
//
// What covers those six is TestCompressibleTypes_CannotCoverSSE rather than
// another route test: all ten share one allowlist, and proving the list
// cannot name event-stream holds for every stream that exists or is added.
func TestSSE_IsNeverCompressedThroughServerHandler(t *testing.T) {
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()

	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	hub := repos.NewTaskHub(context.Background())
	m.Set("beta", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "beta", Hub: hub}))

	s := &Server{
		Manager:        m,
		ClientSessions: newClientSessionsStore(t).WithHub(hubCtx),
		Logs:           logging.NewTap(tapCtx, 10),
	}
	h := s.Handler()

	for _, path := range []string{
		APIBase + "/repo-events",
		APIBase + "/sessions/events",
		APIBase + "/logs/events",
		APIBase + "/repos/beta/branches/agent:test/events",
	} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			rec := newStreamRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
				// Ask for gzip explicitly: this is the request shape that
				// would trip an allowlist carrying text/* or event-stream.
				req.Header.Set("Accept-Encoding", "gzip")
				h.ServeHTTP(rec, req)
			}()
			rec.waitFor(t, "the first frame", func(b string) bool { return strings.Contains(b, "event: ") })

			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Errorf("Content-Type = %q, want text/event-stream", got)
			}
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("stream was compressed: Content-Encoding = %q, want empty", got)
			}
			cancel()
			<-done
		})
	}
}

// The allowlist is the single thing standing between every SSE stream and a
// compressor, including the six no route test reaches. Two shapes would
// silently re-enable compression on text/event-stream, so both are rejected
// here by construction rather than one route at a time:
//
//   - a literal text/event-stream entry;
//   - any "family/*" entry. chi's NewCompressor sorts those into
//     allowedWildcards and matches the whole family, so "text/*" would sweep
//     in event-stream without ever naming it — and would read, to a hurried
//     editor shortening the list, like a tidy-up.
//
// This cannot drift as routes are added, which is the property the route
// tests do not have.
func TestCompressibleTypes_CannotCoverSSE(t *testing.T) {
	if len(compressibleTypes) == 0 {
		t.Fatal("compressibleTypes is empty")
	}
	for _, ct := range compressibleTypes {
		if ct == "text/event-stream" {
			t.Errorf("compressibleTypes contains %q — a compressor buffers, and a buffered SSE stream never arrives", ct)
		}
		if strings.HasSuffix(ct, "/*") {
			t.Errorf("compressibleTypes contains the wildcard %q — chi matches the whole family, which sweeps in text/event-stream", ct)
		}
	}
}
