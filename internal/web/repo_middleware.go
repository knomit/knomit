package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// RepoMiddleware extracts the {repo} URL param, resolves it via the Manager,
// and stores the RepoInstance in the request context for downstream handlers
// to read with repos.RepoFromContext.
//
// This lives in web rather than repos for the same reason BranchMiddleware
// does: it is HTTP code. Its only job beyond a map lookup is rendering a 404,
// and that response must be problem+json like every other API error — which
// would force internal/repos to import internal/web/hal, violating the
// recorded invariant that repos (domain lifecycle) never imports web (HTTP
// presentation). The context accessors stay in repos so internal/mcp can read
// them without importing web.
//
// The 404 body is byte-identical to the hand-written lookups this middleware
// replaced, so wrapping a previously hand-checking route changes nothing an
// API client can observe.
func RepoMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "repo")
			ri := m.Get(name)
			if ri == nil {
				hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
					`no repo named "`+name+`"`, r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(r.Context(), ri)))
		})
	}
}
