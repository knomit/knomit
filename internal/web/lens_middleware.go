package web

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// LensMiddleware resolves the {lens} URL param into a Binding and stores it
// (plus the lens's write repo as the context RepoInstance) in the request
// context.
//
// The resolution itself is domain logic and lives in
// repos.ResolveLensBinding; this middleware is only the HTTP shell that maps
// the classified failure onto a status and a problem+json body. Rendering
// lives here, not in repos, for the reason spelled out on RepoMiddleware.
func LensMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := repos.ResolveLensBinding(r.Context(), m, chi.URLParam(r, "lens"))
			if err != nil {
				status, title := lensResolveStatus(err)
				hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// lensResolveStatus maps a lens-resolution failure onto its HTTP status and
// problem title. An unclassified error is a 500: resolution is exhaustively
// classified, so reaching the default means the classification drifted, and
// reporting that as a server fault is the honest answer.
func lensResolveStatus(err error) (int, string) {
	var re *repos.LensResolveError
	if !errors.As(err, &re) {
		return http.StatusInternalServerError, "Lens registry error"
	}
	switch re.Kind {
	case repos.LensRegistryUnavailable:
		return http.StatusServiceUnavailable, "Lens registry unavailable"
	case repos.LensNotFound:
		return http.StatusNotFound, "Lens not found"
	case repos.LensUnavailable:
		return http.StatusServiceUnavailable, "Lens unavailable"
	default: // repos.LensRegistryError
		return http.StatusInternalServerError, "Lens registry error"
	}
}
