package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newServerWithSessions returns a Server wired with a real SessionManager and
// the given registered repo names, ready for origin-sessions route tests.
func newServerWithSessions(t *testing.T, repoNames ...string) *Server {
	t.Helper()
	return &Server{
		Manager:        newTestManagerWithRepos(t, repoNames...),
		SessionManager: NewSessionManager(),
	}
}

// TestOriginSessions_UnknownRepo_Returns404 verifies that any request against
// /repos/{repo}/origin-sessions/... for an unregistered repo returns 404.
func TestOriginSessions_UnknownRepo_Returns404(t *testing.T) {
	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/repos/missing/origin-sessions", `{"url":"https://github.com/x/y.git"}`},
		{http.MethodGet, "/repos/missing/origin-sessions/abc", ""},
		{http.MethodDelete, "/repos/missing/origin-sessions/abc", ""},
	}

	for _, tc := range endpoints {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			s := newServerWithSessions(t, "alpha")
			r := s.NewAPIRouter()

			var bodyReader *strings.Reader
			if tc.body != "" {
				bodyReader = strings.NewReader(tc.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status: got %d, want 404 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestOriginSessions_CreateSession_Returns200 verifies that POST to
// /repos/{repo}/origin-sessions with a valid body creates a session and
// returns 200 with a session_id.
func TestOriginSessions_CreateSession_Returns200(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	body := `{"url":"https://github.com/example/repo.git","auth_method":"token","token":"tok"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SessionID == "" {
		t.Error("session_id should not be empty")
	}
}

// TestOriginSessions_CreateSession_MissingURL_Returns400 verifies that POST
// without a url field returns 400.
func TestOriginSessions_CreateSession_MissingURL_Returns400(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// TestOriginSessions_CreateSession_InvalidJSON_Returns400 verifies bad JSON
// bodies are rejected.
func TestOriginSessions_CreateSession_InvalidJSON_Returns400(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions", strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
}

// TestOriginSessions_GetSession_Returns200 verifies that a created session can
// be retrieved by ID.
func TestOriginSessions_GetSession_Returns200(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	// Create a session first.
	createBody := `{"url":"https://github.com/example/repo.git","auth_method":"token","token":"tok"}`
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/repos/alpha/origin-sessions", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body=%s", createRec.Code, createRec.Body.String())
	}

	var createResp struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("create unmarshal: %v", err)
	}

	// Fetch the session.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions/"+createResp.SessionID, nil)
	r.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("get: status %d, body=%s", getRec.Code, getRec.Body.String())
	}

	var getResp struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("get unmarshal: %v", err)
	}
	if getResp.SessionID != createResp.SessionID {
		t.Errorf("session_id: got %q, want %q", getResp.SessionID, createResp.SessionID)
	}
}

// TestOriginSessions_GetSession_UnknownID_Returns404 verifies that fetching a
// session that does not exist returns 404.
func TestOriginSessions_GetSession_UnknownID_Returns404(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions/nonexistent", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

// TestOriginSessions_DeleteSession_Returns204 verifies that DELETE on a
// created session returns 204.
func TestOriginSessions_DeleteSession_Returns204(t *testing.T) {
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

	var createResp struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Delete the session.
	delRec := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin-sessions/"+createResp.SessionID, nil)
	r.ServeHTTP(delRec, delReq)

	if delRec.Code != http.StatusNoContent {
		t.Errorf("delete: status %d, want 204", delRec.Code)
	}

	// Confirm it's gone.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/repos/alpha/origin-sessions/"+createResp.SessionID, nil)
	r.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Errorf("after delete get: status %d, want 404", getRec.Code)
	}
}

// TestOriginSessions_DeleteSession_NonExistent_Returns204 verifies that
// deleting an unknown session is idempotent (no error).
func TestOriginSessions_DeleteSession_NonExistent_Returns204(t *testing.T) {
	s := newServerWithSessions(t, "alpha")
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin-sessions/ghost", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want 204", rec.Code)
	}
}
