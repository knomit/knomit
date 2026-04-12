package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// stubFactWriterForCreate is a minimal FactWriter for create handler tests.
type stubFactWriterForCreate struct{}

func (stubFactWriterForCreate) Write(_ *repos.RepoInstance, _, _, _, _ string) (string, error) {
	return "abc123", nil
}
func (stubFactWriterForCreate) Delete(_ *repos.RepoInstance, _, _, _ string) (string, error) {
	return "abc123", nil
}

func TestHandleFactCreate_Returns201WithLocation(t *testing.T) {
	s := &Server{
		Manager:      newTestManagerWithRepos(t, "alpha"),
		OntologyRoot: "know",
		factWriter:   stubFactWriterForCreate{},
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
		factWriter:   stubFactWriterForCreate{},
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
		factWriter:   stubFactWriterForCreate{},
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
		factWriter:   stubFactWriterForCreate{},
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
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
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
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 session, got %d", len(list))
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
