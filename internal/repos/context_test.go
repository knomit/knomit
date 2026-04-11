package repos_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
)

// TestRepoMiddleware_PassesRepoToNestedHandler verifies that a handler
// mounted deep under a chi route with RepoMiddleware receives the repo
// in its request context, even through an intermediate http.HandlerFunc.
// This is the regression gate for the MCP stateless-handler refactor:
// if this breaks, the MCP handlers that read ri from ctx at call time
// will blow up.
func TestRepoMiddleware_PassesRepoToNestedHandler(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{AgentBranch: "agent/test"})
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "testrepo",
		AgentBranch: "agent/test",
	})
	m.Set("testrepo", ri)

	var seen *repos.RepoInstance
	nested := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = repos.RepoFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	r := chi.NewRouter()
	r.Route("/api/v1/{repo}", func(sub chi.Router) {
		sub.Use(repos.RepoMiddleware(m))
		// Mirror the way /mcp is mounted in web/server.go.
		sub.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			nested.ServeHTTP(w, req)
		}))
	})

	req := httptest.NewRequest("POST", "/api/v1/testrepo/mcp", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if seen == nil {
		t.Fatal("RepoFromContext returned nil — middleware did not populate ctx")
	}
	if seen.Name() != "testrepo" {
		t.Errorf("wrong repo: got %q, want %q", seen.Name(), "testrepo")
	}
}

// TestRepoFromContextOpt_MissingRepo verifies the non-panicking variant
// returns (nil, false) when no repo is in context.
func TestRepoFromContextOpt_MissingRepo(t *testing.T) {
	ri, ok := repos.RepoFromContextOpt(context.Background())
	if ok {
		t.Error("expected ok=false for empty context")
	}
	if ri != nil {
		t.Error("expected nil ri for empty context")
	}
}

// TestRepoFromContextOpt_PresentRepo verifies the non-panicking variant
// returns (ri, true) when a repo is in context.
func TestRepoFromContextOpt_PresentRepo(t *testing.T) {
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "example",
		AgentBranch: "agent/test",
	})
	ctx := repos.WithRepoInstance(context.Background(), ri)
	got, ok := repos.RepoFromContextOpt(ctx)
	if !ok {
		t.Error("expected ok=true")
	}
	if got != ri {
		t.Errorf("got different instance: %p vs %p", got, ri)
	}
}
