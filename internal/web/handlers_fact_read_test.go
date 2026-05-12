package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// stubFactReader implements the minimal interface the HAL fact handler needs.
//
// The fallback contract is exercised via three optional fields:
//   - fallbackFact / fallbackHead: when set AND the caller passes fallback=true,
//     Read returns these instead of the primary (fact, head, readErr) tuple.
//     This lets a single test exercise both the primary-success path and the
//     fallback path without duplicating the whole struct.
//   - lastFallback: records the fallback flag the handler actually passed
//     (asserted by tests that care about how the parameter is wired).
type stubFactReader struct {
	fact    knomitfact.Fact
	exists  map[string]bool
	head    string
	readErr error

	// Fallback overrides — used only when the caller passes fallback=true.
	fallbackFact    knomitfact.Fact
	fallbackHead    string
	fallbackErr     error
	useFallbackData bool

	lastFallback bool
}

func (s *stubFactReader) Read(_ *repos.RepoInstance, _ hal.Anchor, _ string, fallback bool) (knomitfact.Fact, string, error) {
	s.lastFallback = fallback
	if fallback && s.useFallbackData {
		return s.fallbackFact, s.fallbackHead, s.fallbackErr
	}
	return s.fact, s.head, s.readErr
}

func (s *stubFactReader) Exists(path string) bool { return s.exists[path] }

func TestHandleHALFact_ReturnsHALEnvelope(t *testing.T) {
	f := knomitfact.NewFact("know/ai/ml/abc12345.md")
	f.Title = "Attention"
	f.Body = "Body"
	f.Type = knomitfact.EpistemicType("observation")
	f.Domain = []string{"ai"}
	f.Entities = []string{"transformer"}
	f.Refs = []string{"know/ai/ml/xyz99999.md"}
	f.Confidence = 0.9
	f.Sources = 2

	reader := &stubFactReader{
		fact:   f,
		exists: map[string]bool{"know/ai/ml/xyz99999.md": true},
		head:   "7f3a8b2c",
	}
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factReader: reader,
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/ai/ml/abc12345.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}

	var body struct {
		Path  string `json:"path"`
		Title string `json:"title"`
		Body  string `json:"body"`
		AsOf  AsOf   `json:"as_of"`
		Refs  []struct {
			Raw   string      `json:"raw"`
			Kind  string      `json:"kind"`
			Links hal.LinkMap `json:"_links,omitempty"`
		} `json:"refs"`
		Links hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Path != "know/ai/ml/abc12345.md" {
		t.Errorf("path: %q", body.Path)
	}
	if body.AsOf.Branch != "agent/test" {
		t.Errorf("as_of.branch: %q", body.AsOf.Branch)
	}
	if body.AsOf.Commit != "7f3a8b2c" {
		t.Errorf("as_of.commit: %q", body.AsOf.Commit)
	}
	if len(body.Refs) != 1 || body.Refs[0].Kind != "fact" {
		t.Errorf("refs: %+v", body.Refs)
	}
	for _, rel := range []string{"self", "incoming", "outgoing", "commits", "snapshot", "parent", "branch"} {
		if _, ok := body.Links[rel]; !ok {
			t.Errorf("missing link %q", rel)
		}
	}
}

func TestHandleHALFact_NotFound_ReturnsProblem(t *testing.T) {
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factReader: &stubFactReader{readErr: errFactNotFound},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/missing.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

// TestHandleHALFact_BackendError_Returns500: regression for the old behavior
// that collapsed ALL reader errors into errFactNotFound (404). Real backend
// failures (e.g. disk error, git corruption) must surface as 500 so users
// know something is broken rather than seeing "no fact at this path".
func TestHandleHALFact_BackendError_Returns500(t *testing.T) {
	s := &Server{
		Manager:    newTestManagerWithRepos(t, "alpha"),
		factReader: &stubFactReader{readErr: errors.New("disk on fire")},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/know/x.md", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for non-notfound error, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}
