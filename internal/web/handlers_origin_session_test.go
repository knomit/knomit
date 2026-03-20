package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCreateSession_ValidInput(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["session_id"] == "" {
		t.Error("expected non-empty session_id")
	}
}

func TestCreateSession_InvalidURL(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"not-a-url","auth_method":"token"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateSession_HTTPWithSSHAuth(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"ssh"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["error"] == "" {
		t.Error("expected error message")
	}
}

func TestCreateSession_ReplacesExisting(t *testing.T) {
	handler := newTestRouter(nil, nil)

	// First session
	rr1 := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr1.Code != http.StatusOK {
		t.Fatalf("first create: expected 200, got %d: %s", rr1.Code, rr1.Body.String())
	}
	var body1 map[string]string
	_ = json.NewDecoder(rr1.Body).Decode(&body1)
	firstID := body1["session_id"]

	// Second session (replaces first)
	rr2 := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo2.git","auth_method":"token","token":"ghp_def"}`)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second create: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}
	var body2 map[string]string
	_ = json.NewDecoder(rr2.Body).Decode(&body2)
	secondID := body2["session_id"]

	if firstID == secondID {
		t.Error("expected different session IDs")
	}

	// First session should be gone (get returns 404)
	rr3 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+firstID, "")
	if rr3.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for replaced session, got %d", rr3.Code)
	}

	// Second session should be accessible
	rr4 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+secondID, "")
	if rr4.Code != http.StatusOK {
		t.Fatalf("expected 200 for current session, got %d: %s", rr4.Code, rr4.Body.String())
	}
}

func TestGetSession_Found(t *testing.T) {
	handler := newTestRouter(nil, nil)

	// Create
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", rr.Code)
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Get
	rr2 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", rr2.Code, rr2.Body.String())
	}

	var getBody map[string]any
	if err := json.NewDecoder(rr2.Body).Decode(&getBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if getBody["session_id"] != sessionID {
		t.Errorf("expected session_id=%s, got %v", sessionID, getBody["session_id"])
	}
	if getBody["state"] != "created" {
		t.Errorf("expected state=created, got %v", getBody["state"])
	}
	if getBody["url"] != "https://github.com/org/repo.git" {
		t.Errorf("expected url, got %v", getBody["url"])
	}
}

func TestGetSession_NotFound(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/nonexistent-id", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDeleteSession(t *testing.T) {
	handler := newTestRouter(nil, nil)

	// Create
	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"url":"https://github.com/org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d", rr.Code)
	}
	var createBody map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&createBody)
	sessionID := createBody["session_id"]

	// Delete
	rr2 := doRequest(t, handler, http.MethodDelete, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", rr2.Code)
	}

	// Get should 404
	rr3 := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin/session/"+sessionID, "")
	if rr3.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rr3.Code)
	}
}

func TestCreateSession_MissingURL(t *testing.T) {
	handler := newTestRouter(nil, nil)

	rr := doRequest(t, handler, http.MethodPost, "/api/v1/knomit/origin/session",
		`{"auth_method":"token","token":"ghp_abc"}`)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

