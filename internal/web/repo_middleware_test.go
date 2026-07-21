package web

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
		sub.Use(RepoMiddleware(m))
		// Mirror the way /mcp is mounted in the API router.
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

// TestRepoMiddleware_UnknownRepoProblemJSON pins the 404 envelope. It must
// stay byte-compatible with the hand-written lookups this middleware
// replaced, since ~34 routes switched from those to this one.
func TestRepoMiddleware_UnknownRepoProblemJSON(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})

	r := chi.NewRouter()
	r.With(RepoMiddleware(m)).Get("/repos/{repo}", func(w http.ResponseWriter, req *http.Request) {
		t.Error("handler ran for an unknown repo")
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/repos/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: got %q, want application/problem+json", got)
	}
}
