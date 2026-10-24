package httpx

import (
	"errors"
	"net/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/index"

	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/facts"
)

// StoreUnavailable reports whether err is RepoInstance.Acquire's refusal —
// the repo's store cannot be reached right now: none is attached
// (repos.ErrStoreUnavailable, which the populating refusal wraps) or the
// instance is being torn down (repos.ErrRepoClosed). RepoInstance.WithRead
// returns the same errors and does NOT run its closure, so a helper that
// ignores WithRead's return turns this state into a zero-valued success.
func StoreUnavailable(err error) bool {
	return errors.Is(err, repos.ErrStoreUnavailable) || errors.Is(err, repos.ErrRepoClosed)
}

// WriteStoreUnavailable is the 503 for StoreUnavailable, in the wording the
// repo PATCH (README.md / LICENSE writes) already answers it with. The detail
// is fixed rather than err.Error(): the error can arrive wrapped by the ref
// gate or a store call, and that chain is not the client's business.
func WriteStoreUnavailable(w http.ResponseWriter, r *http.Request) {
	hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not ready",
		"the repo's store is not available; try again", r.URL.Path)
}

// WriteStoreError maps a store-layer error to a problem+json HTTP response.
// A store that cannot be reached (StoreUnavailable) is 503; branch-not-found
// becomes 404 with a stable title; every other error falls
// through to 500 using the caller-supplied defaultTitle. Handlers pass the
// branch name separately so the 404 detail message is accurate even when the
// error chain doesn't carry it.
func WriteStoreError(w http.ResponseWriter, r *http.Request, err error, defaultTitle, branch string) {
	if StoreUnavailable(err) {
		WriteStoreUnavailable(w, r)
		return
	}
	if errors.Is(err, model.ErrBranchNotFound) {
		hal.WriteProblem(w, http.StatusNotFound, "Branch not found",
			`no branch named "`+branch+`"`, r.URL.Path)
		return
	}
	if errors.Is(err, facts.ErrInvalidPath) {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid path", err.Error(), r.URL.Path)
		return
	}
	// RuleValidation at the source (store validateWrite): the content fails
	// the ontology's validations; the detail is the ontology's own message.
	var invalid *facts.FactInvalidError
	if errors.As(err, &invalid) {
		hal.WriteProblem(w, http.StatusUnprocessableEntity, "Fact fails the ontology's validations",
			invalid.Error(), r.URL.Path)
		return
	}
	if errors.Is(err, index.ErrFactNotLive) {
		hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
			`no live version of this fact on branch "`+branch+`" — it may have been retracted (use the commit-anchored endpoint to read older versions)`,
			r.URL.Path)
		return
	}
	hal.WriteProblem(w, http.StatusInternalServerError, defaultTitle,
		err.Error(), r.URL.Path)
}
