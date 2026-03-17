package web

import (
	"net/http"

	"knomit/internal/embeddings"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/store"
	"knomit/internal/synthesize"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
)

// GitStore is the narrow git interface needed by read-only query handlers
// and the sync task handler. Accepts *git.Store at runtime.
type GitStore interface {
	ListDir(path string) ([]git.DirEntry, error)
	ReadFile(path string) (string, error)
	ReadFileAtCommit(path, commitHash string) (string, error)
	ReadFileLastCommit(path, beforeCommitHash string) (content string, fromCommit string, err error)
	WriteFile(path, content, message string) (commitHash, blobHash string, err error)
	Log(path string) ([]git.LogEntry, error)
	LogPaginated(path string, limit int, after string) ([]git.LogEntryWithTags, string, error)
	CommitDetail(commitHash string) (*git.CommitDetailResult, error)
	Activity(path string) (git.ActivityResult, error)
	HeadCommit() (string, error)
	Branch() string
	ListAll() ([]string, error)
}

// SearchIndex is the narrow search/index interface needed by query handlers.
// Accepts *store.Index at runtime.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	GetLastCommit(branch string) (string, error)
	Stats(pathPrefix string) (store.StatsResult, error)
}

// SynthDeps bundles the dependencies needed by the synthesize handler.
// May be nil if no LLM is configured — the synth handler returns 503
// in that case rather than panicking.
type SynthDeps struct {
	GS       synthesize.GitStore
	Idx      synthesize.SearchIndex
	Embedder *embeddings.Embedder
	Adapter  llm.LLMAdapter
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
func NewRouter(rm *RepoManager, gitHandler http.Handler, embeddingsEnabled bool, ontologyRoot string) http.Handler {
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
		sub.Use(repoMiddleware(rm))
		sub.Get("/browse", handleBrowse(ontologyRoot))
		sub.Get("/fact", handleFact())
		sub.Put("/fact", handleFactWrite())
		sub.Get("/search", handleSearch())
		sub.Get("/history", handleHistoryPaginated())
		sub.Get("/commit", handleCommitDetail())
		sub.Get("/stats", handleStats())
		sub.Get("/activity", handleActivity())
		sub.Get("/status", handleStatus(embeddingsEnabled, ontologyRoot))
		sub.Post("/synthesize", handleSynthesizeStart())
		sub.Get("/events", handleEvents())
		sub.Get("/origin", handleGetOrigin())
		sub.Put("/origin", handleSetOrigin())

		sub.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ri := RepoFromContext(req.Context())
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
