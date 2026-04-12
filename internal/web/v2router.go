package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// V2URLBase is the URL prefix for the API router.
const V2URLBase = "/api/v1"

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

	// Legacy routes — kept under /{repo}/... until each is converted to HAL.
	// Chi matches literal "/repos" before the param "/{repo}", so these
	// coexist with the new HAL routes above without conflict.
	r.Get("/openapi.yaml", handleOpenAPISpec())
	r.Route("/{repo}", func(sub chi.Router) {
		sub.Use(repos.RepoMiddleware(s.Manager))
		sub.Get("/browse", handleBrowse(s.OntologyRoot, s.AgentBranch))
		sub.Get("/fact", handleFact(s.AgentBranch))
		sub.Put("/fact", handleFactWrite(s.AgentBranch))
		sub.Delete("/fact", handleFactRetract(s.AgentBranch))
		sub.Get("/search", handleSearch())
		sub.Get("/explain", handleExplain())
		sub.Get("/history", handleHistoryPaginated(s.AgentBranch))
		sub.Get("/commit", handleCommitDetail(s.AgentBranch))
		sub.Get("/stats", handleStats())
		sub.Get("/activity", handleActivity(s.AgentBranch))
		sub.Get("/status", handleStatus(s.EmbeddingsEnabled, s.OntologyRoot, s.AgentBranch))
		sub.Post("/synthesize", s.handleSynthesizeStart())
		sub.Post("/rebuild", handleRebuild())
		sub.Get("/completions", handleCompletions())
		sub.Get("/recent", handleRecent())
		sub.Get("/events", handleEvents())
		sub.Get("/origin", handleGetOrigin())
		sub.Put("/origin", handleSetOrigin())
		sub.Post("/origin/session", handleCreateSession(s.Manager, s.SessionManager))
		sub.Get("/origin/session/{sessionID}", handleGetSession(s.Manager, s.SessionManager))
		sub.Delete("/origin/session/{sessionID}", handleDeleteSession(s.Manager, s.SessionManager))
		sub.Get("/origin/session/{sessionID}/test", handleTestConnectivity(s.Manager, s.SessionManager, s.AgentBranch))
		sub.Get("/origin/session/{sessionID}/preview", handlePreview(s.Manager, s.SessionManager, s.AgentBranch))
		sub.Post("/origin/session/{sessionID}/apply", handleApply(s.Manager, s.SessionManager, s.AgentBranch))
		sub.Post("/origin/session/{sessionID}/commit", s.handleCommit(s.Manager, s.SessionManager, s.AgentBranch))

		sub.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			profile := req.URL.Query().Get("profile")
			if profile == "" {
				profile = "code"
			}
			h, ok := s.mcpHandlers[profile]
			if !ok {
				h = s.mcpHandlers["code"]
			}
			if h == nil {
				http.NotFound(w, req)
				return
			}
			h.ServeHTTP(w, req)
		}))
	})

	return r
}
