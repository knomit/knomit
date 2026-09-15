package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"knomit/internal/platform/logging"
)

// serveLogEvents runs the stream in a goroutine against the recorder from
// handlers_client_sessions_events_test.go, and returns a cancel plus a wait.
func serveLogEvents(t *testing.T, s *Server) (*streamRecorder, context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/logs/events", nil).WithContext(ctx))
	}()
	return rec, cancel, done
}

func TestLogEvents_ReplaysBacklogThenReadyThenStreamsLive(t *testing.T) {
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()
	tap := logging.NewTap(tapCtx, 100)
	if _, err := tap.Write([]byte("2026-09-15T10:44:55Z INF older\n2026-09-15T10:44:56Z WRN newer\n")); err != nil {
		t.Fatal(err)
	}

	s := &Server{Manager: newTestManagerWithRepos(t), Logs: tap}
	rec, cancel, done := serveLogEvents(t, s)
	defer cancel()

	body := rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}

	// The backlog arrives BEFORE ready, oldest first: a tab that opened after
	// the interesting line was logged is the whole reason the ring exists.
	older := strings.Index(body, "older")
	newer := strings.Index(body, "newer")
	ready := strings.Index(body, "event: ready")
	if older < 0 || newer < 0 || !(older < newer && newer < ready) {
		t.Fatalf("want older < newer < ready, got %d < %d < %d in:\n%s", older, newer, ready, body)
	}

	var got struct {
		Retained int `json:"retained"`
		Max      int `json:"max"`
	}
	if err := json.Unmarshal([]byte(lastDataLine(t, body, "event: ready")), &got); err != nil {
		t.Fatalf("ready payload: %v", err)
	}
	if got.Retained != 2 || got.Max != 100 {
		t.Errorf("ready = %+v, want {retained:2 max:100}", got)
	}

	// Live lines follow.
	if _, err := tap.Write([]byte("2026-09-15T10:45:00Z ERR live-one\n")); err != nil {
		t.Fatal(err)
	}
	body = rec.waitFor(t, "the live line", func(b string) bool { return strings.Contains(b, "live-one") })
	var line struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal([]byte(lastDataLine(t, body, "event: line")), &line); err != nil {
		t.Fatalf("line payload: %v", err)
	}
	if line.Line != "2026-09-15T10:45:00Z ERR live-one" {
		t.Errorf("live line = %q", line.Line)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return when the request context ended")
	}
}

// A line carrying quotes, newlines or an SSE frame delimiter must not be able
// to forge a frame. JSON-encoding the payload is what prevents that.
func TestLogEvents_LineIsJSONEncodedNotInterpolated(t *testing.T) {
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()
	tap := logging.NewTap(tapCtx, 10)
	if _, err := tap.Write([]byte(`2026-09-15T10:44:55Z INF said "hi" data: event: fake` + "\n")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Manager: newTestManagerWithRepos(t), Logs: tap}
	rec, cancel, _ := serveLogEvents(t, s)
	defer cancel()

	body := rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })
	var line struct {
		Line string `json:"line"`
	}
	if err := json.Unmarshal([]byte(lastDataLine(t, body, "event: line")), &line); err != nil {
		t.Fatalf("line payload is not valid JSON: %v\n%s", err, body)
	}
	if !strings.Contains(line.Line, `said "hi"`) {
		t.Errorf("round-tripped line lost its quotes: %q", line.Line)
	}
	// Exactly two events: the injected "event: fake" must be inside a data
	// payload, not a frame of its own.
	if n := strings.Count(body, "\nevent: "); n > 2 {
		t.Errorf("payload forged extra frames (%d event lines):\n%s", n, body)
	}
}

// The read-only demo is public. The server's own log is not part of the demo.
func TestLogEvents_ReadOnlyIs403(t *testing.T) {
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()
	s := &Server{Manager: newTestManagerWithRepos(t), Logs: logging.NewTap(tapCtx, 10), ReadOnly: true}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/logs/events", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "problem+json") {
		t.Errorf("Content-Type: got %q, want problem+json", ct)
	}
	if b := rec.Body.String(); !strings.Contains(b, "Read-only instance") {
		t.Errorf("body does not name the reason: %s", b)
	}
}

// Same contract as the sessions endpoints: no source, no stream.
func TestLogEvents_NoTapIs503(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/logs/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
}

// A viewer that is behind must be TOLD it is behind. Silently resuming after a
// gap presents a discontinuous log as a continuous one.
func TestLogEvents_ReportsDroppedLines(t *testing.T) {
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()
	tap := logging.NewTap(tapCtx, 10)
	s := &Server{Manager: newTestManagerWithRepos(t), Logs: tap}

	// A recorder that blocks the handler's first write until released, so the
	// tap's bounded buffer overflows underneath it.
	rec, cancel, _ := serveLogEvents(t, s)
	defer cancel()
	rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })

	rec.block()
	for i := 0; i < 4000; i++ {
		if _, err := tap.Write([]byte("2026-09-15T10:45:00Z INF flood\n")); err != nil {
			t.Fatal(err)
		}
	}
	rec.unblock()

	body := rec.waitFor(t, "a dropped frame", func(b string) bool { return strings.Contains(b, "event: dropped") })
	var got struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(lastDataLine(t, body, "event: dropped")), &got); err != nil {
		t.Fatalf("dropped payload: %v", err)
	}
	if got.Count <= 0 {
		t.Errorf("dropped count = %d, want > 0", got.Count)
	}
}

// The read-only demo REFUSES the log stream (403), so advertising it from the
// API root would be a link that is always a dead end — discovery promising
// something the server will not do. Absent, not empty: a present-but-blank href
// is a different lie.
func TestAPIRoot_OmitsLogsLinkWhenReadOnly(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t), ReadOnly: true}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var body struct {
		Links map[string]json.RawMessage `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body.Links["logs"]; present {
		t.Errorf("read-only root advertises a logs link that always 403s: %v", body.Links)
	}
	// The rest of discovery is unaffected — this is one link, not a mode.
	for _, rel := range []string{"self", "repos", "version", "openapi"} {
		if _, ok := body.Links[rel]; !ok {
			t.Errorf("read-only root lost the %q link", rel)
		}
	}
}

func TestAPIRoot_LinksToLogs(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var body struct {
		Links map[string]struct {
			Href string `json:"href"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Links["logs"].Href; !strings.HasSuffix(got, "/logs/events") {
		t.Errorf("_links.logs.href = %q, want a /logs/events URL", got)
	}
}

// Same rule as the sessions stream: a client that stops draining must not pin
// the handler. It matters more here — the log is the highest-rate stream the
// server has, so a wedged write backs up fastest, and an unchecked write error
// after a fired deadline would spin on a dead connection at full log rate.
func TestLogEvents_WriteFailureEndsTheHandler(t *testing.T) {
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()
	tap := logging.NewTap(tapCtx, 100)
	s := &Server{Manager: newTestManagerWithRepos(t), Logs: tap}

	rec, cancel, done := serveLogEvents(t, s)
	defer cancel()
	rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })

	rec.mu.Lock()
	deadlines := rec.deadlines
	rec.mu.Unlock()
	if deadlines == 0 {
		t.Fatal("handler set no write deadline")
	}

	rec.failWrites(errors.New("connection reset by peer"))
	if _, err := tap.Write([]byte("2026-09-15T10:45:00Z INF after the break\n")); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept running after a failed write")
	}
}

// The deadline must survive the real middleware chain here too — Compress and
// the metrics middleware both wrap the writer, and a handler that treats an
// unsupported deadline as fatal would otherwise refuse to stream at all.
func TestLogEvents_WorksThroughTheRealMiddlewareChain(t *testing.T) {
	tapCtx, tapCancel := context.WithCancel(context.Background())
	defer tapCancel()
	tap := logging.NewTap(tapCtx, 100)
	if _, err := tap.Write([]byte("2026-09-15T10:44:55Z INF backlog line\n")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Manager: newTestManagerWithRepos(t), Logs: tap}

	srv := httptest.NewServer(s.NewAPIRouter())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/logs/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	want := func(what string, match func(string) bool) {
		t.Helper()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %s", what)
				}
				if match(line) {
					return
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	want("the backlog line", func(l string) bool { return strings.Contains(l, "backlog line") })
	want("the ready frame", func(l string) bool { return l == "event: ready" })
	if _, err := tap.Write([]byte("2026-09-15T10:45:00Z WRN live line\n")); err != nil {
		t.Fatal(err)
	}
	want("the live line", func(l string) bool { return strings.Contains(l, "live line") })
}
