package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// stubFactWriterForCreate is a minimal FactWriter for create handler tests.
// The ref gate sees an empty corpus, which is right for every test here that
// posts a fact with no refs.
type stubFactWriterForCreate struct{}

func (stubFactWriterForCreate) FactResolves(_ context.Context, _ *repos.RepoInstance, _, _ string) (bool, error) {
	return false, nil
}

func (stubFactWriterForCreate) PriorRefs(_ context.Context, _ *repos.RepoInstance, _, _ string) ([]string, error) {
	return nil, nil
}

func (stubFactWriterForCreate) Write(_ context.Context, _ *repos.RepoInstance, _, _, _, _ string) (string, error) {
	return "abc123", nil
}
func (stubFactWriterForCreate) Delete(_ context.Context, _ *repos.RepoInstance, _, _, _ string) (string, error) {
	return "abc123", nil
}

func TestHandleFactCreate_Returns201WithLocation(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers: storeProviders{
			factWriter: stubFactWriterForCreate{},
		},
	}
	r := s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":["ai","ml"],"confidence":0.9,"sources":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Error("Location header missing")
	}
	if ct := rec.Header().Get("Content-Type"); ct != hal.ContentType {
		t.Errorf("Content-Type: got %q, want %q", ct, hal.ContentType)
	}

	var view struct {
		Title  string      `json:"title"`
		Domain []string    `json:"domain"`
		Type   string      `json:"type"`
		Links  hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Title != "My Fact" {
		t.Errorf("title: got %q, want %q", view.Title, "My Fact")
	}
	if _, ok := view.Links["self"]; !ok {
		t.Error("missing self link")
	}
}

func TestHandleFactCreate_MissingTitle_Returns400(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers: storeProviders{
			factWriter: stubFactWriterForCreate{},
		},
	}
	r := s.NewAPIRouter()

	body := `{"body":"no title here"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestHandleFactCreate_UnknownRepo_Returns404(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t),
		OntologyRoot: "know",
		providers: storeProviders{
			factWriter: stubFactWriterForCreate{},
		},
	}
	r := s.NewAPIRouter()

	body := `{"title":"T"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/missing/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleFactCreate_InvalidType_Returns400(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers: storeProviders{
			factWriter: stubFactWriterForCreate{},
		},
	}
	r := s.NewAPIRouter()

	body := `{"title":"T","type":"bogus"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

func TestHandleListSessions_EmptyList_Returns200(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 || len(body.Embedded.Sessions) != 0 {
		t.Errorf("expected empty collection, got count=%d items=%d",
			body.Count, len(body.Embedded.Sessions))
	}
}

func TestHandleListSessions_AfterCreate_Returns1(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	// Create a session.
	createBody := `{"url":"https://github.com/example/repo.git"}`
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body=%s", createRec.Code, createRec.Body.String())
	}

	// List sessions.
	listRec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions", nil)
	r.ServeHTTP(listRec, listReq)

	if listRec.Code != http.StatusOK {
		t.Fatalf("list: status %d, body=%s", listRec.Code, listRec.Body.String())
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Sessions []map[string]any `json:"sessions"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 1 || len(body.Embedded.Sessions) != 1 {
		t.Errorf("expected 1 session, got count=%d items=%d",
			body.Count, len(body.Embedded.Sessions))
	}
}

func TestHandleListSessions_UnknownRepo_Returns404(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/missing/origin-sessions", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestGetOpenAPISpec_Returns200(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type: got %q, want application/yaml", ct)
	}
}

// TestHandleFactCreate_AppliesDefaults pins the same contract knomit_learn
// enforces: a create body that omits confidence and sources must land on the
// documented defaults rather than Go's zero values. A fact written with
// sources 0 contributes nothing to any downstream evidence weight, which
// multiplies by sources.
func TestHandleFactCreate_AppliesDefaults(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: stubFactWriterForCreate{}},
	}
	r := s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":["ai"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Confidence float64 `json:"confidence"`
		Sources    int     `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Sources != 1 {
		t.Errorf("omitted sources: got %d, want the default 1", view.Sources)
	}
	if view.Confidence != 0.7 {
		t.Errorf("omitted confidence: got %v, want the default 0.7", view.Confidence)
	}
}

// TestHandleFactCreate_ExplicitZeroSourcesSurvives guards the other side: an
// explicit 0 is a legal value and must not be defaulted away.
func TestHandleFactCreate_ExplicitZeroSourcesSurvives(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers:    storeProviders{factWriter: stubFactWriterForCreate{}},
	}
	r := s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","sources":0,"confidence":0}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var view struct {
		Confidence float64 `json:"confidence"`
		Sources    int     `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if view.Sources != 0 {
		t.Errorf("explicit 0 sources: got %d, want 0 preserved", view.Sources)
	}
	if view.Confidence != 0 {
		t.Errorf("explicit 0 confidence: got %v, want 0 preserved", view.Confidence)
	}
}

// The create path derives the fact's location from caller-supplied domain, so
// a caller can steer it into a private directory. Accepting that returns 201
// for a fact the indexer, Verify and the exporter all skip — silent loss.
func TestHandleFactCreate_RejectsPrivateDomain(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		providers: storeProviders{
			factWriter: stubFactWriterForCreate{},
		},
	}
	r := s.NewAPIRouter()

	body := `{"title":"My Fact","body":"some body","type":"observation","domain":[".secret"],"confidence":0.9,"sources":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/main/facts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "private") {
		t.Errorf("problem body should name the private-path rule, got %s", rec.Body.String())
	}
}
