package web

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
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

// LensExperimentMiddleware resolves {lens} into a Binding whose WRITE MEMBER
// is re-pinned to exp/{name}, for the /lenses/{lens}/experiments/{name}/mcp
// mount.
//
// WHY A URL SEGMENT AND NOT PER-SESSION STATE. The lens MCP mount is
// URL-scoped, so a `binding` handle is refused on presence
// (kb/invariants/mcp/session-binding/handle-is-the-only-router, rule 3) and
// there is no per-call value that could name the caller. The only ambient key
// left would be Mcp-Session-Id, which is exactly the key the 2026-09-17
// cross-binding incident removed: one connection can carry several logical
// jobs. Putting the experiment in the path makes it part of the endpoint's
// identity, the way the branch segment already is on the repo mount, and
// keeps the server stateless about who is calling.
//
// MIRRORED SURFACE. This sets the same two context values LensMiddleware does
// — the Binding and the write repo as the context RepoInstance — because the
// repo and lens mounts diverge exactly where one of them puts something in
// the context the other does not (kb/architecture/web/096bb34b). The repo
// mount's equivalent needs no middleware at all: its {branch} segment already
// carries exp:<name>, and BindingFromContext synthesizes the lens-of-one from
// the RepoInstance and that branch.
//
// An experiment this lens's write member may NOT write is a 404 rather than a
// silent fall-back to the agent branch. On the session-scoped mount a lapsed
// experiment heals into the ordinary binding because the caller is mid-session
// and its call must still run; here the experiment is the ENDPOINT, so serving
// a different branch than the URL names would be the silent-misroute shape
// this whole design removes.
func LensExperimentMiddleware(m *repos.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			lensName := chi.URLParam(r, "lens")
			expName := chi.URLParam(r, "name")

			reg := m.LensRegistry()
			if reg == nil {
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
					"the lens registry is not open", r.URL.Path)
				return
			}
			l, ok, err := reg.Get(lensName)
			if err != nil {
				hal.WriteProblem(w, http.StatusInternalServerError, "Lens registry error",
					"lens registry: "+err.Error(), r.URL.Path)
				return
			}
			if !ok {
				hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
					"no lens named "+lensName, r.URL.Path)
				return
			}
			write := m.GetByUID(l.WriteUID)
			if write == nil {
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens unavailable",
					"the write member of lens "+lensName+" is not available", r.URL.Path)
				return
			}
			if !write.WritableBranch(store.ExperimentBranch(expName)) {
				hal.WriteProblem(w, http.StatusNotFound, "Experiment not found",
					"no experiment named "+expName+" on repo "+write.Name()+
						" that this instance may write; it may have been committed, rolled back or expired",
					r.URL.Path)
				return
			}
			b, berr := repos.NewBindingOfLensOnExperiment(m, l, expName)
			if berr != nil {
				hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens unavailable",
					berr.Error(), r.URL.Path)
				return
			}
			ctx := repos.WithBinding(r.Context(), b)
			ctx = repos.WithRepoInstance(ctx, b.Write())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
