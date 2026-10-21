package web

import (
	"errors"
	"net/http"

	"knomit/internal/store/index"

	"knomit/internal/store"
	"knomit/internal/store/facts"
	"knomit/internal/web/hal"
)

// writeStoreError maps a store-layer error to a problem+json HTTP response.
// Branch-not-found becomes 404 with a stable title; every other error falls
// through to 500 using the caller-supplied defaultTitle. Handlers pass the
// branch name separately so the 404 detail message is accurate even when the
// error chain doesn't carry it.
func writeStoreError(w http.ResponseWriter, r *http.Request, err error, defaultTitle, branch string) {
	if errors.Is(err, store.ErrBranchNotFound) {
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
