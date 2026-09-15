package web

import (
	"context"
	"encoding/json"
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
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{}, 64)}
}

func (s *streamRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
