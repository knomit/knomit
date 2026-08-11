package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// renameRepoRequest is the POST /repos/{repo}/rename body.
type renameRepoRequest struct {
	Name string `json:"name"`
}

// maxRenameBodyBytes bounds the decoder. A repo name is [a-z0-9_-]+ with no
// length cap, but a legitimate one is tens of bytes; this only stops bodies
// nowhere near a real request.
const maxRenameBodyBytes = 4 << 10

// renameErrStatus maps an error from m.RenameRepo to the HTTP status and
// problem body fields the handler should write. Split out from the handler so
// the ErrRepoNotFound-means-409-here case (see below) can be exercised
// directly, without racing goroutines against the manager just to reach it.
func renameErrStatus(err error, oldName, newName string) (status int, title, detail string) {
	switch {
	case errors.Is(err, repos.ErrInvalidName):
		// 400, not 422: this is a fixed-grammar syntax check on the name
		// itself, matching every other name-grammar rejection in this
		// package — repo create (handlers_repos_create.go), lens
		// create/patch (handlers_lenses_hal.go), archive/restore
		// (handlers_archived_hal.go). 422 here is reserved for
		// cross-referential failures: syntactically fine, wrong against
		// other state (an unknown repo/branch reference, an over-cap
		// description). A bad-characters name is not that.
		return http.StatusBadRequest, "Invalid repo name",
			"a repo name may contain only lowercase letters, digits, hyphens and underscores"
	case errors.Is(err, repos.ErrRepoExists):
		return http.StatusConflict, "Name already in use",
			"another repository is already called \"" + newName + "\""
	case errors.Is(err, repos.ErrRepoNameConflictsLens):
		return http.StatusConflict, "Name already in use",
			"a lens is already called \"" + newName + "\""
	case errors.Is(err, repos.ErrCreateInFlight):
		// The sentinel says "create", but the caller did not ask for one —
		// report what is true for them: the name is being taken.
		return http.StatusConflict, "Name already in use",
			"another operation is currently claiming \"" + newName + "\"; try again"
	case errors.Is(err, repos.ErrRepoNotFound):
		// This looks like the same sentinel RepoMiddleware maps to 404 for a
		// genuinely unknown {repo} — it is NOT the same case. By the time
		// RenameRepo runs, the middleware has already resolved oldName against
		// a live repo; the only way RenameRepo can still return
		// ErrRepoNotFound is that oldName stopped resolving between the
		// middleware's lookup and this call. RenameRepo has two places that can
		// produce that: the forward CAS (RenameIfNamed(oldName, newName))
		// matching zero rows because a concurrent rename beat this one to
		// oldName, or the later revalidate-under-lock step catching Archive/
		// Remove pulling oldName out of the map after this call's own CAS had
		// already landed, in which case it reverts its write before returning
		// — see lifecycle.go's RenameRepo doc comment for both mechanisms. In
		// every case the repo this request named did exist a moment ago, so
		// telling the client "not found" would be false; it changed out from
		// under the request, which is a conflict the client should resolve by
		// re-reading the repo and retrying, not by concluding the name never
		// existed.
		return http.StatusConflict, "Repo changed during rename",
			"repo \"" + oldName + "\" was renamed or removed while this request was in flight; re-read it and retry"
	case errors.Is(err, repos.ErrManagerStopped):
		// Reachable in production, not just in tests: cmd/serve.go's shutdown
		// gives in-flight handlers a 5s grace window between Shutdown and the
		// deferred Close, and a request that lands in that window sees the
		// control.db tenants already nilled out (see controlHandles). That is
		// the same transient, retryable class handleHALRepoPatch already maps
		// to 503 for ErrRepoClosed/ErrStoreUnavailable — a stopped manager
		// deserves the same answer, not a 500 that implies the server is
		// broken rather than mid-restart.
		return http.StatusServiceUnavailable, "Repo not ready",
			"the repo manager is starting up or shutting down; try again"
	default:
		return http.StatusInternalServerError, "Rename failed", "could not rename the repository"
	}
}

// handleHALRepoRename serves POST /api/v1/repos/{repo}/rename.
//
// A custom action rather than PATCH /repos/{repo}: repoView re-reads the repo
// through the name in the URL, and a rename invalidates the very identifier the
// request was addressed by, so the write and its response cannot share one
// resource. The slash form matches the existing resource-level action
// POST /archived/{id}/restore. Deliberately NOT the AIP colon form — that
// convention is scoped to COLLECTION-level actions with no {repo} segment
// (/repos:rescan). Do not "fix" this to /repos/{repo}:rename.
func handleHALRepoRename(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		oldName := chi.URLParam(r, "repo")

		r.Body = http.MaxBytesReader(w, r.Body, maxRenameBodyBytes)
		var req renameRepoRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}
		if req.Name == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing name",
				"a new name is required", r.URL.Path)
			return
		}

		if err := m.RenameRepo(oldName, req.Name); err != nil {
			status, title, detail := renameErrStatus(err, oldName, req.Name)
			if status == http.StatusInternalServerError {
				log.Error().Err(err).Str("from", oldName).Str("to", req.Name).Msg("rename repo failed")
			}
			hal.WriteProblem(w, status, title, detail, r.URL.Path)
			return
		}

		// Re-read under the NEW name so the response — self link included —
		// addresses the repo as it now exists.
		ri := m.Get(req.Name)
		if ri == nil {
			log.Error().Str("to", req.Name).Msg("rename succeeded but the repo is not resolvable")
			hal.WriteProblem(w, http.StatusInternalServerError, "Rename failed",
				"the repository was renamed but could not be re-read", r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, repoView(b, r, req.Name, ri))
	}
}
