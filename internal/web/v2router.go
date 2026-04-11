package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"knomit/internal/web/hal"
)

// V2URLBase is the URL prefix under which the new HAL router is mounted
// during Plans 01 and 02. Plan 03 renames this to "/api/v1" as part of the
// final swap that retires the legacy handlers.
const V2URLBase = "/api/v1-new"

// NewV2Router constructs the HAL v2 chi router for this Server. The router
// is rooted at "/" — the caller mounts it under V2URLBase via chi.Mount.
// Everything below this router assumes V2URLBase as the Server-level prefix.
//
// Middleware order:
//  1. Recoverer — catch panics, return 500 problem+json
//  2. Compress(5) — mandatory per design spec §3 rule 6
//  3. NotFound / MethodNotAllowed handlers — return problem+json
//
// Per-route middleware (BranchMiddleware, RepoMiddleware) is attached at
// the route-group level where branches/repos appear in the path, not at
// the router root.
func (s *Server) NewV2Router() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no resource at "+req.URL.Path, req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed",
			"method "+req.Method+" not allowed on "+req.URL.Path, req.URL.Path)
	})

	b := hal.URLBuilder{Base: V2URLBase}
	r.Get("/", handleV2APIRoot(b))
	r.Get("/repos", handleV2Repos(b, s.Manager))
	r.Get("/repos/{repo}", handleV2Repo(b, s.Manager))

	lister := s.branchesLister
	if lister == nil {
		lister = defaultBranchesLister
	}
	r.Get("/repos/{repo}/branches", handleV2Branches(b, s.Manager, lister))

	reader := s.branchRootReader
	if reader == nil {
		reader = defaultBranchRootReader
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}",
		handleV2Branch(b, s.Manager, reader, s.AgentBranch, s.EmbeddingsEnabled),
	)

	factReader := s.factReader
	if factReader == nil {
		factReader = defaultFactReader{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/facts/*",
		handleV2Fact(b, s.Manager, factReader),
	)

	return r
}
