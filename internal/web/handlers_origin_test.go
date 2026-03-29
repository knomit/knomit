package web

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
)

func TestHandleGetOrigin_NoService(t *testing.T) {
	// ri.Svc is nil → 204
	handler := newTestRouter(nil, nil)
	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
}

func TestHandleGetOrigin_NoRemote(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "knomit",
		Svc:  svc,
		Hub:  hub,
	}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
}

func TestHandleGetOrigin_WithRemote(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	if err := svc.SetRemote("origin", "https://github.com/org/kb.git", "main", 300, 0); err != nil {
		t.Fatalf("SetRemote: %v", err)
	}

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "knomit",
		Svc:  svc,
		Hub:  hub,
	}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodGet, "/api/v1/knomit/origin", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var remote store.Remote
	if err := json.NewDecoder(rr.Body).Decode(&remote); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if remote.Name != "origin" {
		t.Errorf("expected name=origin, got %q", remote.Name)
	}
	if remote.URL != "https://github.com/org/kb.git" {
		t.Errorf("expected url=https://github.com/org/kb.git, got %q", remote.URL)
	}
	if remote.Branch != "main" {
		t.Errorf("expected branch=main, got %q", remote.Branch)
	}
}

func TestHandleSetOrigin_MissingURL(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "knomit",
		Svc:  svc,
		Hub:  hub,
	}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/origin", `{"auth_method":"token"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["error"] != "url is required" {
		t.Errorf("unexpected error: %q", body["error"])
	}
}

func TestHandleSetOrigin_ValidURL(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "knomit",
		Svc:  svc,
		Hub:  hub,
	}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/origin", `{"url":"https://github.com/org/new-kb.git","auth_method":"token","token":"ghp_abc"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", body["status"])
	}
	if body["branch"] != "main" {
		t.Errorf("expected branch=main, got %q", body["branch"])
	}

	// Verify it was persisted
	remote, err := svc.GetRemote("origin")
	if err != nil {
		t.Fatalf("GetRemote: %v", err)
	}
	if remote == nil {
		t.Fatal("expected remote to be saved")
	}
	if remote.URL != "https://github.com/org/new-kb.git" {
		t.Errorf("expected url=https://github.com/org/new-kb.git, got %q", remote.URL)
	}
}

func TestHandleSetOrigin_SSHUrl(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "knomit", Svc: svc, Hub: hub}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/origin", `{"url":"git@github.com:knomit/blank.git","auth_method":"ssh"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSetOrigin_HTTPWithSSHAuth(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "knomit", Svc: svc, Hub: hub}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/origin", `{"url":"https://github.com/org/repo.git","auth_method":"ssh"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSetOrigin_SSHWithTokenAuth(t *testing.T) {
	svc, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer svc.Close()

	hub := repos.NewTaskHub(context.Background())
	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "knomit", Svc: svc, Hub: hub}))
	handler := NewRouter(rm, nil, false, "kb", testAgentBranch)

	rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/origin", `{"url":"git@github.com:org/repo.git","auth_method":"token","token":"ghp_abc"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHandleSetOrigin_NoService(t *testing.T) {
	handler := newTestRouter(nil, nil)
	rr := doRequest(t, handler, http.MethodPut, "/api/v1/knomit/origin", `{"url":"https://example.com/repo.git"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}
