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

// renameLensRequest is the POST /lenses/{lens}/rename body.
type renameLensRequest struct {
	Name string `json:"name"`
}

// lensRenameErrStatus maps an error from m.RenameLens to the HTTP status and
// problem body fields the handler should write. Mirrors renameErrStatus in
// handlers_repos_rename.go arm-for-arm where the reasoning transfers; see the
// ErrLensNotFound arm below for the one place it does not.
func lensRenameErrStatus(err error, oldName, newName string) (status int, title, detail string) {
	switch {
	case errors.Is(err, repos.ErrInvalidLensName):
		// 400, not 422: this is a fixed-grammar syntax check on the name
		// itself, matching every other name-grammar rejection in this
		// package — repo create (handlers_repos_create.go), lens
		// create/patch (handlers_lenses_hal.go), archive/restore
		// (handlers_archived_hal.go). 422 here is reserved for
		// cross-referential failures: syntactically fine, wrong against
		// other state (an unknown repo/branch reference, an over-cap
		// description). A bad-characters name is not that.
		return http.StatusBadRequest, "Invalid lens name",
			"a lens name may contain only lowercase letters, digits, hyphens and underscores"
	case errors.Is(err, repos.ErrLensNameConflictsRepo):
		return http.StatusConflict, "Name already in use",
			"a repo is already called \"" + newName + "\""
	case errors.Is(err, repos.ErrLensExists):
		return http.StatusConflict, "Name already in use",
			"another lens is already called \"" + newName + "\""
	case errors.Is(err, repos.ErrCreateInFlight):
		// RenameLens reserves newName in the same in-flight set repo Create
		// reserves into, so an in-flight create/restore/lens-create claiming
		// the target surfaces here. Same wording as the repo route: the
		// sentinel says "create", but the caller did not ask for one — report
		// what is true for them, that the name is being taken.
		return http.StatusConflict, "Name already in use",
			"another operation is currently claiming \"" + newName + "\"; try again"
	case errors.Is(err, repos.ErrLensNotFound):
		// On the repo side, ErrRepoNotFound reaching renameErrStatus means
		// RepoMiddleware already resolved oldName before the handler ran, so
		// the only way to still see it is a concurrent change — hence 409,
		// not 404. That reasoning does NOT transfer here: GET/PATCH/DELETE
		// /lenses/{lens}, and this rename route registered alongside them in
		// router.go, sit OUTSIDE the LensMiddleware group — that middleware
		// wraps only the binding-dependent subroutes (facts, search, stats,
		// topics, mcp). Nothing resolves {lens} before this handler runs; it
		// is the first and only lookup, exactly like handleHALLens,
		// handleHALLensPatch and handleHALLensDelete, which all answer an
		// unknown name with a plain 404. So does this one.
		//
		// (RenameLens's own doc comment notes a second source of this same
		// sentinel — LensRegistry.Rename's CAS reporting !changed — but calls
		// it unreachable today: the whole function runs under one
		// m.mu.Lock(), so nothing can move oldName between the Get and the
		// Rename call. No race to attribute differently here.)
		return http.StatusNotFound, "Lens not found",
			"no lens named \"" + oldName + "\""
	default:
		return http.StatusInternalServerError, "Rename failed", "could not rename the lens"
	}
}

// handleHALLensRename serves POST /api/v1/lenses/{lens}/rename.
//
// Same reasoning as handleHALRepoRename: not PATCH /lenses/{lens} — the
// response re-reads the lens through the NEW name, and a rename invalidates
// the very identifier the request was addressed by, so the write and its
// response cannot share one resource. Slash form, matching
// POST /repos/{repo}/rename and POST /archived/{id}/restore. Deliberately
// NOT the AIP colon form (/lenses/{lens}:rename) — that convention is scoped
// to COLLECTION-level actions with no {lens} segment. Do not "fix" this to
// /lenses/{lens}:rename.
//
// Registered in router.go alongside the lens CRUD trio (GET/PATCH/DELETE),
// not inside the LensMiddleware group: like them, it resolves {lens} itself
// and attributes its own not-found/conflict errors — see the ErrLensNotFound
// arm of lensRenameErrStatus for why that matters for the status code.
//
// The registry-unavailable check up front mirrors every other lens handler
// (handleHALLens, handleHALLensPatch, handleHALLensDelete, ...): unlike
// RenameRepo, RenameLens never calls controlHandles() and so never returns
// ErrManagerStopped — an unopened registry surfaces from it as a bare
// non-sentinel error ("lens registry not open"). Checking here, the same way
// the siblings do, is what turns that into a 503 instead of falling through
// lensRenameErrStatus's default 500 arm.
func handleHALLensRename(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		oldName := chi.URLParam(r, "lens")

		r.Body = http.MaxBytesReader(w, r.Body, maxRenameBodyBytes)
		var req renameLensRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}
		if req.Name == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing name",
				"a new name is required", r.URL.Path)
			return
		}

		if err := m.RenameLens(oldName, req.Name); err != nil {
			status, title, detail := lensRenameErrStatus(err, oldName, req.Name)
			if status == http.StatusInternalServerError {
				log.Error().Err(err).Str("from", oldName).Str("to", req.Name).Msg("rename lens failed")
			}
			hal.WriteProblem(w, status, title, detail, r.URL.Path)
			return
		}

		// Re-read under the NEW name so the response — self link included —
		// addresses the lens as it now exists.
		l, ok, err := reg.Get(req.Name)
		if err != nil {
			log.Error().Err(err).Str("to", req.Name).Msg("get lens after rename failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Rename failed",
				"the lens was renamed but could not be re-read", r.URL.Path)
			return
		}
		if !ok {
			log.Error().Str("to", req.Name).Msg("rename succeeded but the lens is not resolvable")
			hal.WriteProblem(w, http.StatusInternalServerError, "Rename failed",
				"the lens was renamed but could not be re-read", r.URL.Path)
			return
		}
		writeLensView(w, r, b, m.Repos(), http.StatusOK, l)
	}
}
