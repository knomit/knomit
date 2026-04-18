package web

import (
	"errors"
	"net/http"

	"knomit/internal/store"
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
	hal.WriteProblem(w, http.StatusInternalServerError, defaultTitle,
		err.Error(), r.URL.Path)
}
