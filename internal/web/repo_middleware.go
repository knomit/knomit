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
//
// A name that IS registered but has no live store is a 409, not a 404. The two
// answer different questions — "there is no such repo" versus "this repo cannot
// be served right now" — and only the second leaves the caller anything to do.
// The body names the reason, because the fixes diverge: a missing file wants a
// restore, an unopenable one wants a look at the log, and a conflict wants the
// other copy of that knowledge base dealt with first.
func RepoMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "repo")
			ri := m.Get(name)
			if ri == nil {
				if u, ok := unavailableByName(m, name); ok {
					hal.WriteProblem(w, http.StatusConflict, "Repo unavailable",
						`repo "`+name+`" is registered but has no store (`+u.Reason+`): `+u.Detail,
						r.URL.Path)
					return
				}
				hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
					`no repo named "`+name+`"`, r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(repos.WithRepoInstance(r.Context(), ri)))
		})
	}
}

// unavailableByName finds the registered-but-storeless repo called name.
//
// A linear scan over Unavailable(): the slice is empty on a healthy instance and
// holds one entry per BROKEN repo otherwise, so an index would be a map that is
// almost always empty, kept in sync for a lookup that only runs on the miss path
// of a request that is already about to fail.
func unavailableByName(m *repos.Manager, name string) (repos.Unavailable, bool) {
	for _, u := range m.Unavailable() {
		if u.Record.Name == name {
			return u, true
		}
	}
	return repos.Unavailable{}, false
}
