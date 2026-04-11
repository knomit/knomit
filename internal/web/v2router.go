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

	// Routes for specific resources are registered in Milestones 4-6.
	return r
}
