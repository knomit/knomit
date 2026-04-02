package web

import (
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
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

// SetupMCP wires MCP handlers onto ri using the server's ontology and deps.
// Safe to call after SwapStore to rebind MCP handlers to the new database.
func (s *Server) SetupMCP(ri *repos.RepoInstance) {
	ontology := s.Manager.Ontology()
	if ontology == nil {
		return
	}

	var svc *store.Service
	ri.WithRead(func(d repos.StoreDeps) { svc = d.Svc })
	if svc == nil {
		log.Warn().Msg("SetupMCP: svc is nil, skipping")
		return
	}
	idx := svc.Index()

	reviewer := synthesize.NewReviewer(svc, idx, idx, s.Embedder, nil, s.AgentBranch)
	profiles := []string{"code", "chat", "generic"}
	mcpHandlers := make(map[string]http.Handler, len(profiles))
	for _, p := range profiles {
		var mcpSrv *mcpserver.MCPServer
		if s.Embedder != nil {
			mcpSrv = mcp.NewServer(svc, idx, idx, idx, reviewer, p, s.OntologyRoot, ontology, s.AgentBranch, s.Embedder)
		} else {
			mcpSrv = mcp.NewServer(svc, idx, idx, idx, reviewer, p, s.OntologyRoot, ontology, s.AgentBranch)
		}
		mcpHandlers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
	}

	ri.SetMCPHandlers(mcpHandlers)
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
		sub.Post("/origin/session/{sessionID}/commit", s.handleCommit(s.Manager, s.SessionManager, s.AgentBranch))

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

