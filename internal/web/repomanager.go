package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
	"knomit/internal/store"
)

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	Name       string
	GS         GitStore
	Svc        *store.Service
	Idx        SearchIndex
	Hub        *TaskHub
	SyncCancel context.CancelFunc
	SyncWg     *sync.WaitGroup
}

// RepoManager is a concurrent-safe registry of named RepoInstances.
type RepoManager struct {
	mu    sync.RWMutex
	repos map[string]*RepoInstance
}

// NewRepoManager returns an empty RepoManager.
func NewRepoManager() *RepoManager {
	return &RepoManager{repos: make(map[string]*RepoInstance)}
}

// Get returns the RepoInstance for name, or nil if not found.
func (rm *RepoManager) Get(name string) *RepoInstance {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.repos[name]
}

// Set registers a RepoInstance under the given name.
func (rm *RepoManager) Set(name string, ri *RepoInstance) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.repos[name] = ri
}

// Replace swaps the RepoInstance for name and returns the old instance
// (or nil if there was none) so the caller can clean it up.
func (rm *RepoManager) Replace(name string, ri *RepoInstance) *RepoInstance {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	old := rm.repos[name]
	rm.repos[name] = ri
	return old
}

// ForEach calls fn for every registered repo while holding a read lock.
func (rm *RepoManager) ForEach(fn func(name string, ri *RepoInstance)) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	for name, ri := range rm.repos {
		fn(name, ri)
	}
}

// ---------- context helpers ----------

type contextKey string

const repoInstanceKey contextKey = "repoInstance"

// WithRepoInstance stores a RepoInstance in the context.
func WithRepoInstance(ctx context.Context, ri *RepoInstance) context.Context {
	return context.WithValue(ctx, repoInstanceKey, ri)
}

// RepoFromContext retrieves the RepoInstance from the request context.
// Panics if not present (middleware must always set it).
func RepoFromContext(ctx context.Context) *RepoInstance {
	ri, ok := ctx.Value(repoInstanceKey).(*RepoInstance)
	if !ok {
		panic("RepoFromContext: no RepoInstance in context")
	}
	return ri
}

// ---------- middleware ----------

// repoMiddleware extracts the {repo} URL param, resolves it via the
// RepoManager, and stores the RepoInstance in the request context.
// Returns a 404 JSON error if the repo is not found.
func repoMiddleware(rm *RepoManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "repo")
			ri := rm.Get(name)
			if ri == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "repository not found: " + name,
				})
				return
			}
			ctx := WithRepoInstance(r.Context(), ri)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
