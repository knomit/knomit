package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// changesPageSize is the default and maximum page size, the same as the MCP
// tool's.
const changesPageSize = 100

// changesView is the HAL page for GET …/changes. Head is always present,
// including on an empty page: it is the caller's next `since`.
type changesView struct {
	Count    int                           `json:"count"`
	Head     string                        `json:"head"`
	HasMore  bool                          `json:"has_more"`
	Links    hal.LinkMap                   `json:"_links"`
	Embedded map[string][]store.PathChange `json:"_embedded"`
}

// handleHALChanges serves GET /repos/{repo}/branches/{branch}/changes: each
// fact path under `prefix` that differs between commit `since` and the
// branch's head, as added/modified/deleted, plus that head. A read of two
// trees — no timestamp, no write. The repo and branch are the ones in the URL;
// a cursor records both and is refused on any other, and `since` also takes
// the MCP tool's `<repo12>:<commit40>` bookmark when it names this repo.
func handleHALChanges(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		branch := BranchFromContext(r.Context())
		qp := r.URL.Query()

		limit := changesPageSize
		if v := qp.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > changesPageSize {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"limit must be an integer from 1 to "+strconv.Itoa(changesPageSize), r.URL.Path)
				return
			}
			limit = n
		}

		// The repo's wire id: what a cursor and a bookmark name it by.
		if ri.ID() == "" {
			hal.WriteProblem(w, http.StatusConflict, "Repo identity unresolved",
				"this repo's identity is not resolved yet (no root commit); retry after the repo has its first commit", r.URL.Path)
			return
		}
		repo12 := federate.ID12(ri.ID())

		var q store.ChangesQuery
		if c := qp.Get("cursor"); c != "" {
			var (
				scope store.ChangesScope
				err   error
			)
			if q, scope, err = store.DecodeChangesCursor(c); err != nil {
				writeChangesError(w, r, err, branch)
				return
			}
			// A cursor minted for another repo or branch is refused, not
			// replayed here. (The store's ancestry check on its pinned head
			// cannot catch this: a branch may contain another's head.)
			if scope.Repo != repo12 || scope.Branch != branch {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid cursor",
					"cursor is for branch "+strconv.Quote(scope.Branch)+" of repo "+scope.Repo+", this route reads branch "+
						strconv.Quote(branch)+" of repo "+repo12+"; restart without a cursor", r.URL.Path)
				return
			}
		} else {
			q = store.ChangesQuery{Since: qp.Get("since"), Prefix: qp.Get("prefix")}
		}
		q.Limit = limit

		// since may be the MCP tool's bookmark (<repo12>:<commit40>); it must
		// name this repo.
		sinceRepo, sinceCommit, err := store.ParseChangesSince(q.Since)
		if err != nil {
			writeChangesError(w, r, err, branch)
			return
		}
		if sinceRepo != "" && sinceRepo != repo12 {
			hal.WriteProblem(w, http.StatusBadRequest, "Bookmark for another repo",
				"bookmark is for repo "+sinceRepo+", this route reads repo "+repo12+" ("+repoName+"); read that repo's route or omit since", r.URL.Path)
			return
		}
		q.Since = sinceCommit

		var res store.ChangesResult
		if aerr := ri.WithRead(func(svc *store.Service) {
			res, err = svc.Facts().ChangesUnder(r.Context(), branch, q)
		}); aerr != nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Store unavailable", aerr.Error(), r.URL.Path)
			return
		}
		if err != nil {
			writeChangesError(w, r, err, branch)
			return
		}

		base := b.Branch(repoName, hal.Anchor{Branch: branch}) + "/changes"
		links := hal.LinkMap{"self": {Href: selfWithQuery(base, r)}}
		if res.HasMore {
			next := store.EncodeChangesCursor(store.ChangesScope{Repo: repo12, Branch: branch}, q.Since, res.Head, q.Prefix, res.Changes[len(res.Changes)-1].Path)
			nq := r.URL.Query()
			nq.Del("since")
			nq.Del("prefix")
			nq.Set("cursor", next)
			links["next"] = hal.Link{Href: base + "?" + nq.Encode()}
		}
		hal.WriteHAL(w, http.StatusOK, changesView{
			Count:    len(res.Changes),
			Head:     res.Head,
			HasMore:  res.HasMore,
			Links:    links,
			Embedded: map[string][]store.PathChange{"changes": res.Changes},
		})
	}
}

// writeChangesError maps the changes read's client errors to 4xx. None of
// them may become an empty page: an empty page reads as "nothing changed".
func writeChangesError(w http.ResponseWriter, r *http.Request, err error, branch string) {
	switch {
	case errors.Is(err, store.ErrSinceNotBehind):
		hal.WriteProblem(w, http.StatusConflict, "since is not behind head", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrUnknownSince):
		hal.WriteProblem(w, http.StatusBadRequest, "Unknown since", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrInvalidPrefix):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid prefix", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrInvalidChangesCursor):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid cursor", err.Error(), r.URL.Path)
	default:
		writeStoreError(w, r, err, "Failed to compute changes", branch)
	}
}
