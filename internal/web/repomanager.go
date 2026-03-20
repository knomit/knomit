package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/git"
	"knomit/internal/store"
)

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	Name        string
	DBPath      string // path to the SQLite database file
	GS          GitStore
	Svc         *store.Service
	Idx         SearchIndex
	Hub         *TaskHub
	SyncCancel  context.CancelFunc
	SyncWg      *sync.WaitGroup
	MCPHandlers map[string]http.Handler // profile -> MCP handler
	SynthDeps   *SynthDeps             // nil if no LLM configured

	// StartSync is called by the origin handler to activate sync/push loops
	// after a remote is configured. Set by main.go during initialization.
	// Takes the remote URL and returns an error if activation fails.
	StartSync func(url string) error
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

// SwapStore replaces the repo's SQLite database with the one from tempDBPath.
// It stops sync loops, closes the old DB, copies the temp file over the real
// one, and reopens store.Service + git.Store from the real path.
// If DBPath is empty (in-memory/test), it falls back to a pointer swap.
func (rm *RepoManager) SwapStore(ri *RepoInstance, tempDBPath string) error {
	// Stop existing sync loops so no goroutines reference the old store.
	if ri.SyncCancel != nil {
		ri.SyncCancel()
	}
	if ri.SyncWg != nil {
		ri.SyncWg.Wait()
	}

	// In-memory / test mode: no persistent DB file, open the temp DB directly.
	// The temp dir must NOT be cleaned up in this case (caller's responsibility).
	if ri.DBPath == "" {
		svc, err := store.Open(tempDBPath)
		if err != nil {
			// Fallback: no file-based swap possible. Keep existing Svc.
			log.Warn().Err(err).Msg("SwapStore: cannot open temp DB, keeping existing service")
			return nil
		}
		gs, err := git.OpenWithStorer(svc.GitStorer())
		if err != nil {
			svc.Close()
			log.Warn().Err(err).Msg("SwapStore: cannot open temp git, keeping existing service")
			return nil
		}
		ri.Svc = svc
		ri.GS = gs
		ri.Idx = svc.Index()
		return nil
	}

	// Close the old database.
	if ri.Svc != nil {
		ri.Svc.Close()
	}

	// Copy temp DB over the real one (backup first).
	backupPath := ri.DBPath + ".bak"
	if err := copyFile(ri.DBPath, backupPath); err != nil {
		// Non-fatal: we can proceed without a backup.
		log.Warn().Err(err).Msg("SwapStore: backup failed")
	}
	if err := copyFile(tempDBPath, ri.DBPath); err != nil {
		// Restore from backup.
		_ = copyFile(backupPath, ri.DBPath)
		return fmt.Errorf("SwapStore: copy temp DB: %w", err)
	}

	// Reopen from the real path.
	svc, err := store.Open(ri.DBPath)
	if err != nil {
		// Restore from backup.
		_ = copyFile(backupPath, ri.DBPath)
		return fmt.Errorf("SwapStore: reopen store: %w", err)
	}

	gs, err := git.OpenWithStorer(svc.GitStorer())
	if err != nil {
		svc.Close()
		_ = copyFile(backupPath, ri.DBPath)
		return fmt.Errorf("SwapStore: reopen git: %w", err)
	}

	ri.Svc = svc
	ri.GS = gs
	ri.Idx = svc.Index()

	// Clean up backup — swap succeeded.
	os.Remove(backupPath)
	return nil
}

// copyFile copies src to dst, creating dst if needed.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
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
