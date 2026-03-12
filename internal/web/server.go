package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"knomit/internal/git"
	"knomit/internal/store"
)

// GitStore is the narrow git interface needed by web handlers.
type GitStore interface {
	ListDir(path string) ([]git.DirEntry, error)
	ReadFile(path string) (string, error)
	Log(path string) ([]git.LogEntry, error)
	HeadCommit() (string, error)
	Branch() string
	ListAll() ([]string, error)
	Sync(remoteAuth interface{}) (git.SyncResult, error)
}

// SearchIndex is the narrow search interface needed by web handlers.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	GetLastCommit() (string, error)
}

// SynthRunner is the interface for triggering and monitoring synthesis runs.
type SynthRunner interface {
	Start(recipe string) (string, error) // returns run ID
	Status(id string) ([]string, bool)   // returns events, done
}

// NewRouter creates and returns the chi router with all API routes registered.
// gitHandler, if non-nil, is mounted at /git and serves the Smart HTTP git protocol.
func NewRouter(gs GitStore, idx SearchIndex, synth SynthRunner, mcpHandler http.Handler, gitHandler http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	if mcpHandler != nil {
		r.Mount("/mcp", mcpHandler)
	}

	if gitHandler != nil {
		r.Mount("/git", gitHandler)
	}

	r.Get("/api/v1/browse", handleBrowse(gs))
	r.Get("/api/v1/fact", handleFact(gs))
	r.Get("/api/v1/search", handleSearch(idx))
	r.Get("/api/v1/history", handleHistory(gs))
	r.Get("/api/v1/stats", handleStats(gs))
	r.Get("/api/v1/status", handleStatus(gs, idx))
	r.Post("/api/v1/synthesize", handleSynthesizeStart(synth))
	r.Get("/api/v1/synthesize/{recipe}", handleSynthesizeStatus(synth))
	r.Post("/api/v1/sync", handleSync(gs))
	r.Get("/api/v1/events", handleEvents(gs, idx))
	r.Get("/api/v1/openapi.yaml", handleOpenAPISpec())
	r.Get("/docs", handleSwaggerUI())

	// Serve embedded web UI
	staticHandler := StaticHandler()
	r.Handle("/assets/*", staticHandler)
	r.Get("/*", newSPAHandler(staticHandler))

	return r
}
