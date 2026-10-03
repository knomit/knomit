package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/platform/version"
	"knomit/tools/bridge/knomitapi"
)

// versionServer answers GET /api/v1/version with full; any other path 404s.
func versionServer(t *testing.T, full string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "x", "commit": "y", "full": full})
	}))
	t.Cleanup(s.Close)
	return s
}

// A mismatch is ONE line naming both versions and the server.
//
// SABOTAGE S2a (compare always equal) → no line → red.
func TestWarnVersionSkew_MismatchIsOneLine(t *testing.T) {
	s := versionServer(t, "0.5.3.newer")
	var w bytes.Buffer
	warnVersionSkew(&w, s.Client(), s.URL, "srv-name", "0.5.3.older")
	got := w.String()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", got)
	}
	for _, want := range []string{"0.5.3.older", "0.5.3.newer", "srv-name"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q does not name %q", got, want)
		}
	}
}

// The same build prints nothing.
//
// SABOTAGE S2b (print even on a match) → red.
func TestWarnVersionSkew_MatchIsSilent(t *testing.T) {
	s := versionServer(t, "0.5.3.same")
	var w bytes.Buffer
	warnVersionSkew(&w, s.Client(), s.URL, "srv", "0.5.3.same")
	if w.Len() != 0 {
		t.Fatalf("matching versions must print nothing, got %q", w.String())
	}
}

// No comparison, no warning: an error status, a body without a version, an
// unreachable server — and a server that hangs, which must not hold the
// check past its client's timeout.
//
// SABOTAGE S2c (print on error) → red.
func TestWarnVersionSkew_NoSignalIsSilent(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"500":        func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"no json":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"empty full": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"full":""}`)) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(h)
			t.Cleanup(s.Close)
			var w bytes.Buffer
			warnVersionSkew(&w, s.Client(), s.URL, "srv", "0.5.3.mine")
			if w.Len() != 0 {
				t.Fatalf("no version to compare must print nothing, got %q", w.String())
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		s := httptest.NewServer(http.NotFoundHandler())
		url := s.URL
		s.Close()
		var w bytes.Buffer
		warnVersionSkew(&w, &http.Client{Timeout: time.Second}, url, "srv", "0.5.3.mine")
		if w.Len() != 0 {
			t.Fatalf("got %q", w.String())
		}
	})
	t.Run("hung server is bounded", func(t *testing.T) {
		release := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			_, _ = w.Write([]byte(`{"full":"0.5.3.other"}`))
		}))
		t.Cleanup(func() { close(release); s.Close() })
		var w bytes.Buffer
		start := time.Now()
		warnVersionSkew(&w, knomitapi.NewServerClient(serverFor(t, s.URL), versionCheckTimeout), s.URL, "srv", "0.5.3.mine")
		if d := time.Since(start); d > versionCheckTimeout+500*time.Millisecond {
			t.Fatalf("the check took %v, past its %v bound", d, versionCheckTimeout)
		}
		if w.Len() != 0 {
			t.Fatalf("a timed-out check must print nothing, got %q", w.String())
		}
	})
}

// serverFor is the named knomitapi.Server for an httptest URL, built the way
// kb builds one from its argument.
func serverFor(t *testing.T, raw string) knomitapi.Server {
	t.Helper()
	isolateServerEnv(t)
	s, err := knomitapi.ResolveServer(raw)
	if err != nil {
		t.Fatalf("ResolveServer(%q): %v", raw, err)
	}
	return s
}

// syncBuffer is a bytes.Buffer safe for the version goroutine to write while
// the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The proxy path end to end against a server of ANOTHER build: stdout holds
// exactly the MCP response line and nothing else; the warning lands on the
// error writer only.
func TestServeProxy_VersionSkewNeverTouchesStdout(t *testing.T) {
	orig, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = orig, origCommit })
	version.Version, version.Commit = "0.5.3", "mine"

	f := &fakeKnomit{name: "h"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"full":"0.5.3.theirs"}`))
	})
	mux.Handle("/", f.handler())
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	srv := serverFor(t, s.URL)
	conn, err := connect(srv, modeRepo, "core", "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var out bytes.Buffer
	var errw syncBuffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	if err := serveProxy(srv, conn, in, &out, &errw); err != nil {
		t.Fatalf("serveProxy: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"h"}}}` + "\n"
	if out.String() != want {
		t.Fatalf("stdout must be exactly the MCP stream:\n got %q\nwant %q", out.String(), want)
	}
	deadline := time.Now().Add(versionCheckTimeout + time.Second)
	for !strings.Contains(errw.String(), "0.5.3.theirs") {
		if time.Now().After(deadline) {
			t.Fatalf("no version warning on the error writer; got %q", errw.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(out.String(), "differs") {
		t.Fatal("the warning leaked onto stdout")
	}
}
