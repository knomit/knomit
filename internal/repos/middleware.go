package repos

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// RepoMiddleware extracts the {repo} URL param, resolves it via the Manager,
// and stores the RepoInstance in the request context.
// Returns a 404 JSON error if the repo is not found.
func RepoMiddleware(m *Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "repo")
			ri := m.Get(name)
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
