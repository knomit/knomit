package web

import (
	"net/http"

	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
)

// Server holds server-wide state for the HTTP layer.
type Server struct {
	Manager           *repos.Manager
	GitHandler        http.Handler
	EmbeddingsEnabled bool
	OntologyRoot      string
	AgentBranch       string
	SessionManager    *SessionManager
	LLMAdapter        llm.LLMAdapter     // nil if no LLM configured
	Embedder          store.BatchEmbedder // nil if unavailable
}

// Handler returns the chi router with all routes mounted.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	if s.GitHandler != nil {
		log.Info().Msg("git handler enabled at /git")
		r.Mount("/git", s.GitHandler)
	}

	r.Get("/api/v1/openapi.yaml", handleOpenAPISpec())
	r.Get("/api/v1/repos", handleRepos(s.Manager))
	r.Get("/docs", handleSwaggerUI())

	r.Route("/api/v1/{repo}", func(sub chi.Router) {
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
		sub.Post("/origin/session/{sessionID}/commit", handleCommit(s.Manager, s.SessionManager, s.AgentBranch))

		sub.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ri := repos.RepoFromContext(req.Context())
			var handler http.Handler
			ri.WithRead(func(d repos.StoreDeps) {
				if len(d.MCP) == 0 {
					return
				}
				profile := req.URL.Query().Get("profile")
				if profile == "" {
					profile = "code"
				}
				h, ok := d.MCP[profile]
				if !ok {
					h = d.MCP["code"]
				}
				handler = h
			})
			if handler == nil {
				http.NotFound(w, req)
				return
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
	s := &Server{
		Manager:           rm,
		GitHandler:        gitHandler,
		EmbeddingsEnabled: embeddingsEnabled,
		OntologyRoot:      ontologyRoot,
		AgentBranch:       agentBranch,
		SessionManager:    sm,
	}
	return s.Handler()
}
