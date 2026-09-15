package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/client/sessions"
)

// streamRecorder is the flusher recorder from handlers_events_test.go with a
// lock and a signal, because this test reads the body from one goroutine
// while the handler writes it from another — a plain ResponseRecorder races.
type streamRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushed chan struct{}
	// writeErr, when set, fails every Write — a client whose socket is gone.
	writeErr error
	// deadlines counts SetWriteDeadline calls. httptest.ResponseRecorder does
	// not implement it, so without the method below the handler's deadline
	// path would report ErrNotSupported in every test and never be exercised.
	deadlines int
}

// SetWriteDeadline makes the recorder satisfy what http.ResponseController
// looks for, so the handler's deadline path runs here as it does in a server.
func (s *streamRecorder) SetWriteDeadline(time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadlines++
	return nil
}

// failWrites makes every subsequent Write fail, as a write to a closed socket
// does.
func (s *streamRecorder) failWrites(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeErr = err
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 64)}
}

func (s *streamRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.ResponseRecorder.Write(b)
}

func (s *streamRecorder) Flush() {
	select {
	case s.flushed <- struct{}{}:
	default:
	}
}

func (s *streamRecorder) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Body.String()
}

// waitFor polls the recorded body until cond holds, so the test never sleeps
// a fixed interval waiting for a frame that is already there.
func (s *streamRecorder) waitFor(t *testing.T, what string, cond func(string) bool) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if b := s.body(); cond(b) {
			return b
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s; body so far:\n%s", what, s.body())
		case <-s.flushed:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestSessionEvents_ReadyThenChangePerWrite(t *testing.T) {
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	store := newClientSessionsStore(t).WithHub(hubCtx)
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), ClientSessions: store}
	r := s.NewAPIRouter()

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/events", nil).WithContext(reqCtx))
	}()

	// A fresh connection announces itself, so the client can tell a new
	// stream (re-read: events may have been missed) from a keepalive.
	rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", cc)
	}

	if err := store.Touch(context.Background(), sessions.Observation{
		SessionID: "s1", Binding: "repo:u-alpha", RemoteIP: "1.2.3.4", UserAgent: "ua", Now: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	body := rec.waitFor(t, "a session frame", func(b string) bool { return strings.Contains(b, "event: session") })
	frame := lastDataLine(t, body, "event: session")
	var got sessions.Change
	if err := json.Unmarshal([]byte(frame), &got); err != nil {
		t.Fatalf("session frame %q: %v", frame, err)
	}
	if got.ID != "s1" || got.Kind != "touch" {
		t.Errorf("session frame = %+v, want {s1 touch}", got)
	}

	// The operator's machine must never travel on this stream: it is allowed
	// under ReadOnly precisely because it carries nothing to redact.
	for _, leak := range []string{"1.2.3.4", "\"ua\"", "u-alpha", "remote_addr", "user_agent"} {
		if strings.Contains(body, leak) {
			t.Errorf("stream leaked %q:\n%s", leak, body)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return when the request context ended")
	}
}

// lastDataLine returns the data payload of the last frame with the given
// event line.
func lastDataLine(t *testing.T, body, event string) string {
	t.Helper()
	frames := strings.Split(body, "\n\n")
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if !strings.Contains(f, event) {
			continue
		}
		for _, line := range strings.Split(f, "\n") {
			if strings.HasPrefix(line, "data: ") {
				return strings.TrimPrefix(line, "data: ")
			}
		}
	}
	t.Fatalf("no %q frame with data in:\n%s", event, body)
	return ""
}

// Same contract as the list handler: no registry, no stream.
func TestSessionEvents_NoStoreIs503(t *testing.T) {
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha")}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/events", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
}

// The stream is discoverable from the collection, not from a path the client
// has to know how to spell.
func TestSessionsCollection_LinksToEvents(t *testing.T) {
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), ClientSessions: newClientSessionsStore(t)}
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	var body struct {
		Links map[string]struct {
			Href string `json:"href"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got := body.Links["events"].Href; !strings.HasSuffix(got, "/sessions/events") {
		t.Errorf("_links.events.href = %q, want a /sessions/events URL", got)
	}
}

// A client that stops draining must not pin the handler forever. goob's pipe
// never blocks Publish and buffers without limit, and the server runs SSE with
// WriteTimeout 0 deliberately — so without a per-write deadline a wedged write
// holds the goroutine and the subscription open indefinitely while events pile
// up behind it. The deadline alone is not enough either: unchecked write errors
// would turn a fired deadline into a loop spinning on a dead connection at full
// event rate.
func TestSessionEvents_WriteFailureEndsTheHandler(t *testing.T) {
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	store := newClientSessionsStore(t).WithHub(hubCtx)
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), ClientSessions: store}
	r := s.NewAPIRouter()

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/events", nil).WithContext(reqCtx))
	}()
	rec.waitFor(t, "the ready frame", func(b string) bool { return strings.Contains(b, "event: ready") })

	// The deadline path must actually have run — otherwise this test would
	// prove nothing about it.
	rec.mu.Lock()
	deadlines := rec.deadlines
	rec.mu.Unlock()
	if deadlines == 0 {
		t.Fatal("handler set no write deadline")
	}

	rec.failWrites(errors.New("connection reset by peer"))
	if err := store.Touch(context.Background(), sessions.Observation{SessionID: "s1", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Returning is what ends the subscription: net/http cancels the request
	// context on handler return, and that is what goob's Subscribe(ctx) waits
	// on to unregister.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler kept running after a failed write — it will spin on a dead connection at full event rate")
	}
}

// The deadline has to survive this server's REAL middleware chain. Compress
// and the metrics middleware both wrap the ResponseWriter, and
// http.NewResponseController only reaches the connection through Unwrap — so a
// wrapper that does not implement it turns every deadline into ErrNotSupported,
// and a handler that treats that as fatal would refuse to stream at all.
//
// Run over a real server, because httptest.NewRequest exercises none of that.
func TestSessionEvents_WorksThroughTheRealMiddlewareChain(t *testing.T) {
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	store := newClientSessionsStore(t).WithHub(hubCtx)
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), ClientSessions: store}

	srv := httptest.NewServer(s.NewAPIRouter())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/sessions/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip") // make Compress wrap the writer
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

	want("the ready frame", func(l string) bool { return l == "event: ready" })
	if err := store.Touch(context.Background(), sessions.Observation{SessionID: "s1", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	want("a session frame", func(l string) bool { return l == "event: session" })
	want("its payload", func(l string) bool { return strings.Contains(l, `"kind":"touch"`) })
}
