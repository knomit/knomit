package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/repos"
)

func TestHandleRepos(t *testing.T) {
	ctrl := gomock.NewController(t)
	gs := NewMockGitStore(ctrl)

	rm := repos.New(context.Background(), repos.Deps{})
	rm.Set("knomit", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "knomit", AgentBranch: "agent/abc123", GS: gs}))
	rm.Set("work", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "work", AgentBranch: "agent/abc123", GS: gs}))

	handler := handleRepos(rm)
	req := httptest.NewRequest("GET", "/api/v1/repos", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var repos []struct {
		Name   string `json:"name"`
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(w.Body).Decode(&repos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	// Sorted alphabetically
	if repos[0].Name != "knomit" || repos[1].Name != "work" {
		t.Errorf("repos = %v, want [knomit, work]", repos)
	}
	if repos[0].Branch != "agent/abc123" {
		t.Errorf("branch = %q, want %q", repos[0].Branch, "agent/abc123")
	}
}

func TestHandleRepos_Empty(t *testing.T) {
	rm := repos.New(context.Background(), repos.Deps{})

	handler := handleRepos(rm)
	req := httptest.NewRequest("GET", "/api/v1/repos", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var repos []struct{ Name string }
	if err := json.NewDecoder(w.Body).Decode(&repos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(repos) != 0 {
		t.Fatalf("got %d repos, want 0", len(repos))
	}
}
