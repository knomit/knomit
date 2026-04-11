package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// newTestManagerWithRepos builds a repos.Manager and registers empty
// RepoInstance stubs under the given names. Tests use this to verify the
// handler's collection shape without spinning up real stores.
func newTestManagerWithRepos(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	for _, name := range names {
		m.Set(name, &repos.RepoInstance{})
	}
	return m
}

func TestHandleV2Repos_ReturnsCollection(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t, "alpha", "beta")}
	r := s.NewV2Router()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: %q", got)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Repos []struct {
				Name  string      `json:"name"`
				Links hal.LinkMap `json:"_links"`
			} `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if body.Count != 2 {
		t.Errorf("count: got %d, want 2", body.Count)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if len(body.Embedded.Repos) != 2 {
		t.Fatalf("embedded repos: got %d, want 2", len(body.Embedded.Repos))
	}

	// Each embedded item carries ONLY _links.self (hard rule §3 #7).
	for _, repo := range body.Embedded.Repos {
		if len(repo.Links) != 1 {
			t.Errorf("repo %q: got %d links, want 1 (self only)", repo.Name, len(repo.Links))
		}
		if _, ok := repo.Links["self"]; !ok {
			t.Errorf("repo %q: missing self link", repo.Name)
		}
	}
}

func TestHandleV2Repos_EmptyManagerReturnsEmptyCollection(t *testing.T) {
	s := &Server{Manager: newTestManagerWithRepos(t)}
	r := s.NewV2Router()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body struct {
		Count    int `json:"count"`
		Embedded struct {
			Repos []any `json:"repos"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 0 {
		t.Errorf("count: got %d, want 0", body.Count)
	}
	if body.Embedded.Repos == nil {
		t.Error("embedded repos should be an empty array, not nil")
	}
}
