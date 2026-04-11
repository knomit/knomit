package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// stubBranchLister is a tiny stub of store.BranchIndex used by the branches
// handler tests. Only ListBranches is exercised here; other methods panic
// because they should never be called from these tests.
type stubBranchLister struct {
	store.BranchIndex
	branches []store.Branch
}

func (s *stubBranchLister) ListBranches(_ context.Context) ([]store.Branch, error) {
	return s.branches, nil
}

// newTestRepoWithBranches stashes a stub BranchIndex on a RepoInstance so
// the branches handler can read it via the standard path. In the real code,
// RepoInstance.WithRead exposes a *store.Service whose Branches() returns a
// BranchIndex; for the test we intercept at the handler's entry point by
// overriding the per-request branch lister via the test-only context helper
// below.
//
// TODO(plan-02): replace this shim with a proper mock once uber-go/mock
// generation is wired up for store interfaces.
func newTestRepoWithBranches(_ *testing.T, _ []store.Branch) *repos.RepoInstance {
	return &repos.RepoInstance{}
}

func TestHandleV2Branches_ReturnsCollection(t *testing.T) {
	// Wire a manager with one repo.
	m := newTestManagerWithRepos(t, "alpha")
	s := &Server{
		Manager: m,
		branchesLister: func(_ *repos.RepoInstance) ([]store.Branch, error) {
			return []store.Branch{
				{Name: "agent/test"},
				{Name: "main"},
			}, nil
		},
	}
	r := s.NewV2Router()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/repos/alpha/branches", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Branches []struct {
				Name  string      `json:"name"`
				Links hal.LinkMap `json:"_links"`
			} `json:"branches"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d", body.Count)
	}
	if len(body.Embedded.Branches) != 2 {
		t.Fatalf("embedded: %d", len(body.Embedded.Branches))
	}

	// The branch named "agent/test" must appear with its self link
	// URL-encoded to "agent:test".
	found := false
	for _, br := range body.Embedded.Branches {
		if br.Name == "agent/test" {
			found = true
			want := V2URLBase + "/repos/alpha/branches/agent:test"
			if got := br.Links["self"].Href; got != want {
				t.Errorf("self: got %q, want %q", got, want)
			}
		}
	}
	if !found {
		t.Error("agent/test not in collection")
	}
}
