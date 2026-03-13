package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"knomit/internal/embeddings"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// GitStore is the narrow git interface needed by read-only query handlers
// and the sync task handler. Accepts *git.Store at runtime.
type GitStore interface {
	ListDir(path string) ([]git.DirEntry, error)
	ReadFile(path string) (string, error)
	Log(path string) ([]git.LogEntry, error)
	HeadCommit() (string, error)
	Branch() string
	ListAll() ([]string, error)
	Sync(remoteAuth interface{}) (git.SyncResult, error)
}

// SearchIndex is the narrow search/index interface needed by query handlers.
// Accepts *store.Index at runtime.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	GetLastCommit() (string, error)
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
//	GET  /api/v1/browse      — directory listing
//	GET  /api/v1/fact        — single fact content
//	GET  /api/v1/search      — vector similarity search
//	GET  /api/v1/history     — git log
//	GET  /api/v1/stats       — aggregate statistics
//	GET  /api/v1/status      — head commit, branch, index state
//	POST /api/v1/synthesize  — start async synthesis task
//	POST /api/v1/sync        — start async git sync task
//	GET  /api/v1/events      — SSE event stream
//	GET  /api/v1/openapi.yaml — OpenAPI spec
//	GET  /docs               — Swagger UI
//	/mcp                     — MCP protocol endpoints (per-profile)
//	/git                     — Smart HTTP git remote
//	/*                       — Embedded SPA with client-side routing fallback
func NewRouter(gs GitStore, idx SearchIndex, hub *TaskHub, synthDeps *SynthDeps, mcpHandlers map[string]http.Handler, gitHandler http.Handler, embeddingsEnabled bool, ontologyRoot string) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	if len(mcpHandlers) > 0 {
		r.Mount("/mcp", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			profile := req.URL.Query().Get("profile")
			if profile == "" {
				profile = "code"
			}
			handler, ok := mcpHandlers[profile]
			if !ok {
				handler = mcpHandlers["code"]
			}
			handler.ServeHTTP(w, req)
		}))
	}

	if gitHandler != nil {
		r.Mount("/git", gitHandler)
	}

	r.Get("/api/v1/browse", handleBrowse(gs, ontologyRoot))
	r.Get("/api/v1/fact", handleFact(gs))
	r.Get("/api/v1/search", handleSearch(idx))
	r.Get("/api/v1/history", handleHistory(gs))
	r.Get("/api/v1/stats", handleStats(gs))
	r.Get("/api/v1/status", handleStatus(gs, idx, embeddingsEnabled))
	r.Post("/api/v1/synthesize", handleSynthesizeStart(synthDeps, hub))
	r.Post("/api/v1/sync", handleSync(gs, hub))
	r.Get("/api/v1/events", handleEvents(gs, idx, hub))
	r.Get("/api/v1/openapi.yaml", handleOpenAPISpec())
	r.Get("/docs", handleSwaggerUI())

	// Serve embedded web UI
	staticHandler := StaticHandler()
	r.Handle("/assets/*", staticHandler)
	r.Get("/*", newSPAHandler(staticHandler))

	return r
}
