package web

import (
	"net/http"

	"knomit/internal/repos"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
)

// NewRouter creates the chi router with all API routes, MCP endpoints,
// git smart-HTTP remote, and the embedded SPA frontend.
//
// Route layout:
//
//	GET  /api/v1/{repo}/browse      — directory listing
//	GET  /api/v1/{repo}/fact        — single fact content
//	GET  /api/v1/{repo}/search      — vector similarity search
//	GET  /api/v1/{repo}/history     — git log
//	GET  /api/v1/{repo}/stats       — aggregate statistics
//	GET  /api/v1/{repo}/status      — head commit, branch, index state
//	POST /api/v1/{repo}/synthesize  — start async synthesis task
//	GET  /api/v1/{repo}/events      — SSE event stream
//	GET  /api/v1/openapi.yaml       — OpenAPI spec
//	GET  /docs                      — Swagger UI
//	/api/v1/{repo}/mcp              — MCP protocol endpoints (per-profile)
//	/git                            — Smart HTTP git remote
//	/*                              — Embedded SPA with client-side routing fallback
func NewRouter(rm *repos.Manager, gitHandler http.Handler, embeddingsEnabled bool, ontologyRoot, agentBranch string) http.Handler {
	return NewRouterWithSessionManager(rm, gitHandler, embeddingsEnabled, ontologyRoot, agentBranch, NewSessionManager())
}

// NewRouterWithSessionManager is like NewRouter but accepts an external SessionManager,
// useful for testing where the test needs direct access to the session manager.
func NewRouterWithSessionManager(rm *repos.Manager, gitHandler http.Handler, embeddingsEnabled bool, ontologyRoot, agentBranch string, sm *SessionManager) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	if gitHandler != nil {
		log.Info().Msg("git handler enabled at /git")
		r.Mount("/git", gitHandler)
	}

	r.Get("/api/v1/openapi.yaml", handleOpenAPISpec())
	r.Get("/api/v1/repos", handleRepos(rm))
	r.Get("/docs", handleSwaggerUI())

	r.Route("/api/v1/{repo}", func(sub chi.Router) {
		sub.Use(repos.RepoMiddleware(rm))
		sub.Get("/browse", handleBrowse(ontologyRoot, agentBranch))
		sub.Get("/fact", handleFact(agentBranch))
		sub.Put("/fact", handleFactWrite(agentBranch))
		sub.Delete("/fact", handleFactRetract(agentBranch))
		sub.Get("/search", handleSearch())
		sub.Get("/explain", handleExplain())
		sub.Get("/history", handleHistoryPaginated(agentBranch))
		sub.Get("/commit", handleCommitDetail(agentBranch))
		sub.Get("/stats", handleStats())
		sub.Get("/activity", handleActivity(agentBranch))
		sub.Get("/status", handleStatus(embeddingsEnabled, ontologyRoot, agentBranch))
		sub.Post("/synthesize", handleSynthesizeStart())
		sub.Post("/rebuild", handleRebuild())
		sub.Get("/completions", handleCompletions())
		sub.Get("/recent", handleRecent())
		sub.Get("/events", handleEvents())
		sub.Get("/origin", handleGetOrigin())
		sub.Put("/origin", handleSetOrigin())
		sub.Post("/origin/session", handleCreateSession(rm, sm))
		sub.Get("/origin/session/{sessionID}", handleGetSession(rm, sm))
		sub.Delete("/origin/session/{sessionID}", handleDeleteSession(rm, sm))
		sub.Get("/origin/session/{sessionID}/test", handleTestConnectivity(rm, sm, agentBranch))
		sub.Get("/origin/session/{sessionID}/preview", handlePreview(rm, sm, agentBranch))
		sub.Post("/origin/session/{sessionID}/apply", handleApply(rm, sm, agentBranch))
		sub.Post("/origin/session/{sessionID}/commit", handleCommit(rm, sm, agentBranch))

		sub.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ri := repos.RepoFromContext(req.Context())
			if len(ri.MCPHandlers) == 0 {
				http.NotFound(w, req)
				return
			}
			profile := req.URL.Query().Get("profile")
			if profile == "" {
				profile = "code"
			}
			handler, ok := ri.MCPHandlers[profile]
			if !ok {
				handler = ri.MCPHandlers["code"]
			}
			handler.ServeHTTP(w, req)
		}))
	})

	// Serve embedded web UI
	staticHandler := StaticHandler()
	r.Handle("/assets/*", staticHandler)
	r.Get("/*", newSPAHandler(staticHandler))

	return r
}
