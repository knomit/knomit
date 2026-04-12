package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
)

// flusher wraps ResponseRecorder to satisfy the http.Flusher interface.
// The SSE handler checks for this interface and will return 500 without it.
type flusher struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flusher) Flush() { f.flushed++ }

func TestHandleHALEvents_SetsContentTypeAndStreams(t *testing.T) {
	hub := repos.NewTaskHub(context.Background())
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha",
		Hub:  hub,
	})
	m := newTestManagerWithRepos(t) // empty manager
	m.Set("alpha", ri)

	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := &flusher{ResponseRecorder: httptest.NewRecorder()}

	// Cancel the request context immediately so the SSE loop exits.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/events", nil).WithContext(ctx)
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type: got %q, want %q", got, "text/event-stream")
	}
	// Body must contain at least the initial status snapshot line.
	body := rec.Body.String()
	if len(body) == 0 {
		t.Error("expected non-empty SSE body (initial status event)")
	}
}

func TestHandleHALEvents_UnknownRepoReturns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/events", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}
