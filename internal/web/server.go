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

	r.Get("/api/browse", handleBrowse(gs))
	r.Get("/api/fact", handleFact(gs))
	r.Get("/api/search", handleSearch(idx))
	r.Get("/api/history", handleHistory(gs))
	r.Get("/api/stats", handleStats(gs))
	r.Get("/api/status", handleStatus(gs, idx))
	r.Post("/api/synthesize", handleSynthesizeStart(synth))
	r.Get("/api/synthesize/{recipe}", handleSynthesizeStatus(synth))

	return r
}
