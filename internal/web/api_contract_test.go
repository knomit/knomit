package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/web/hal"
)

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.Body.String())
	}
	return p
}

// Regression anchors for the API contract that the web-consolidation pass
// unified: one problem+json error envelope, one repo-lookup path, one set of
// query-parameter semantics. Each test here failed before that pass; the one
// that passed before (envelope stability) is pinned precisely because it must
// NOT change.

// ANCHOR 1: envelope unification — origin-sessions (middleware-wrapped today).
func TestAnchor_RepoMiddleware_UnknownRepo_ProblemJSON(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/missing/origin-sessions", nil))

	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
	p := problemOf(t, rec)
	if p["title"] != "Repo not found" {
		t.Errorf("title: got %v, want %q", p["title"], "Repo not found")
	}
	if p["detail"] != `no repo named "missing"` {
		t.Errorf("detail: got %v", p["detail"])
	}
}

// ANCHOR 1b: lens envelope.
func TestAnchor_LensMiddleware_UnknownLens_ProblemJSON(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/lenses/missing/facts", nil))

	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}

// ANCHOR 2: envelope stability on a previously hand-written route.
func TestAnchor_EnvelopeStability_HandWrittenRoute(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/missing/branches/main/search", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
	p := problemOf(t, rec)
	if p["title"] != "Repo not found" || p["detail"] != `no repo named "missing"` {
		t.Errorf("envelope drift: %v", p)
	}
}

// ANCHOR 3: jobs 503 → 404 flip.
func TestAnchor_StartSynthesis_UnknownRepoNoLLM_Returns404(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t), LLMAdapter: nil}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/repos/missing/branches/main/synthesis-runs", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// ANCHOR 4b: DELETE keeps archiveErrStatus attribution, not the middleware 404.
func TestAnchor_RepoArchive_UnknownRepo_KeepsOwnEnvelope(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha")}
	r := s.NewAPIRouter()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/repos/missing", nil))

	p := problemOf(t, rec)
	if p["title"] == "Repo not found" {
		t.Errorf("archive lost its own error attribution: %v", p)
	}
}

// ANCHOR 5: limit unification at the endpoint level.
func TestAnchor_FactsCollection_LimitStrict(t *testing.T) {
	for _, q := range []string{"limit=abc", "limit=0", "limit=-5", "limit=501"} {
		t.Run(q, func(t *testing.T) {
			s := &Server{
				Manager: newTestManagerWithRepos(t, "alpha"),
				providers: storeProviders{
					factsCollection: &stubFactsCollectionProvider{},
				},
			}
			r := s.NewAPIRouter()
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/repos/alpha/branches/main/facts?"+q, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnchor_Search_LimitStrict(t *testing.T) {
	for _, q := range []string{"q=x&limit=-5", "q=x&limit=501"} {
		t.Run(q, func(t *testing.T) {
			s := &Server{
				Manager: newTestManagerWithRepos(t, "alpha"),
				providers: storeProviders{
					search: &stubSearchProvider{},
				},
			}
			r := s.NewAPIRouter()
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/repos/alpha/branches/main/search?"+q, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// ANCHOR 6: origin-session errors are problem+json (Part 4).
func TestAnchor_OriginSession_ConflictIsProblemJSON(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	// Create a session, then commit it before it has been applied.
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions",
		strings.NewReader(`{"url":"https://github.com/example/repo.git"}`))
	createReq.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", createRec.Code, createRec.Body.String())
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/repos/alpha/origin-sessions/"+created.SessionID+"/commit", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
	p := problemOf(t, rec)
	if p["title"] != "Session state conflict" {
		t.Errorf("title: got %v", p["title"])
	}
	if p["detail"] != "session must be in applied state" {
		t.Errorf("detail: got %v", p["detail"])
	}
}

// ANCHOR 6b: origin-session success bodies are HAL with a self link.
func TestAnchor_OriginSession_CreateIsHAL(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions",
		strings.NewReader(`{"url":"https://github.com/example/repo.git"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}
	var body struct {
		SessionID string `json:"session_id"`
		Links     struct {
			Self struct {
				Href string `json:"href"`
			} `json:"self"`
		} `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.SessionID == "" {
		t.Error("session_id missing")
	}
	want := "/api/v1/repos/alpha/origin-sessions/" + body.SessionID
	if body.Links.Self.Href != want {
		t.Errorf("self: got %q, want %q", body.Links.Self.Href, want)
	}
}
