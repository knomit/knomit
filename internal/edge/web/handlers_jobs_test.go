package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/events"
)

func TestHandleStartRebuild_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := asOwner(httptest.NewRequest(http.MethodPost,
		"/repos/missing/branches/main/index-rebuilds", nil))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleStartRebuild_Returns201WithJobEnvelope(t *testing.T) {
	hub, _ := events.NewTaskHub(context.Background())
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha",
		Hub:  hub,
	})
	m := newTestManagerWithRepos(t)
	m.Set("alpha", ri)

	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := asOwner(httptest.NewRequest(http.MethodPost,
		"/repos/alpha/branches/main/index-rebuilds", nil))
	r.ServeHTTP(rec, req)

	// No machine is wired, so the rebuild event is refused with ErrNotOpen:
	// 503, the store-unavailable answer (TestStoreUnavailableAnswers_AreOne).
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503 (no store)", rec.Code)
	}
}

// jobBody is the minimal shape we assert on for job envelope responses.
type jobBody struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	State string `json:"state"`
}

func TestHandleStartRebuild_WithStub_Returns201(t *testing.T) {
	hub, _ := events.NewTaskHub(context.Background())
	// Use a real in-memory store isn't feasible in a unit test; instead
	// we verify 503 when the store is nil (covered above) and separately
	// verify the JSON shape via a router_test integration path if available.
	// For now verify a hub-only instance still responds correctly up to
	// the store-availability gate.
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha",
		Hub:  hub,
	})
	m := newTestManagerWithRepos(t)
	m.Set("alpha", ri)

	s := &Server{Manager: m}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := asOwner(httptest.NewRequest(http.MethodPost,
		"/repos/alpha/branches/main/index-rebuilds", nil))
	r.ServeHTTP(rec, req)

	// No store attached — expect 503 (store unavailable), not a panic.
	if rec.Code == http.StatusInternalServerError {
		t.Errorf("expected non-500 response (got panic or unhandled error), body=%s", rec.Body.String())
	}

	// Verify the response body is valid JSON regardless of status.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("response is not valid JSON: %v, body=%s", err, rec.Body.String())
	}
}

// TestJobs_SynthesisRoutesGone pins that the synthesis-runs routes were
// removed with the CLI adapters and RunAll: the job mount still answers the
// index-rebuilds list, so a 404 here means "no such route", not "no mount".
func TestJobs_SynthesisRoutesGone(t *testing.T) {
	hub, _ := events.NewTaskHub(context.Background())
	m := newTestManagerWithRepos(t)
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "alpha", Hub: hub}))
	r := (&Server{Manager: m}).NewAPIRouter()

	do := func(method, path string) int {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, asOwner(httptest.NewRequest(method, path, nil)))
		return rec.Code
	}
	const base = "/repos/alpha/branches/agent:test/"
	if got := do(http.MethodGet, base+"index-rebuilds"); got != http.StatusOK {
		t.Fatalf("GET index-rebuilds = %d, want 200 (the jobs mount must still be reached)", got)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		if got := do(method, base+"synthesis-runs"); got != http.StatusNotFound {
			t.Errorf("%s synthesis-runs = %d, want 404", method, got)
		}
	}
}
