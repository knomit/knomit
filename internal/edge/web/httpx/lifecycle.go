package httpx

import (
	"context"
	"errors"
	"net/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

// cancelIndexURL is the cancel-index action for a repo: a POST on a
// colon-suffixed sub-resource, the same spelling as /repo-creates/{id}:cancel
// (kb/decisions/repos/create-job/cancel-is-a-post-action).
func cancelIndexURL(b hal.URLBuilder, repo string) string {
	return b.Repo(repo) + "/index:cancel"
}

// WriteLifecycleProblem maps a lifecycle machine's refusal onto HTTP.
//
//   - ErrIndexing → 409 "Repo is indexing", with a cancel-index link: the
//     caller waits, or cancels indexing and retries.
//   - ErrNotOpen → 503 "Repo not ready" with the fixed detail
//     (WriteStoreUnavailable): the store is not attached (a repo still being
//     created, or one whose store is being reopened). Transient. It is the
//     machine's name for the state Acquire refuses with ErrStoreUnavailable,
//     so the client gets the one answer the web layer gives for it. (A bare
//     test instance has no machine and answers ErrNotOpen whatever its store;
//     every instance a running Manager builds has one.)
//   - ErrClosed / ErrUnavailable → 503: the repo is going away or could not be
//     built; the repo row says why.
//   - the request's own cancellation → 503 as well: nothing was decided.
//
// Anything else is the event's own error, and the caller maps it first.
func WriteLifecycleProblem(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, repo string, err error) {
	switch {
	case errors.Is(err, repos.ErrIndexing):
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Repo is indexing",
			"wait for it to finish or cancel indexing", r.URL.Path,
			map[string]any{"_links": map[string]any{
				"cancel-index": map[string]string{"href": cancelIndexURL(b, repo), "method": http.MethodPost},
			}})
	case errors.Is(err, repos.ErrNotOpen):
		WriteStoreUnavailable(w, r)
	case errors.Is(err, repos.ErrClosed), errors.Is(err, repos.ErrUnavailable):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo unavailable", err.Error(), r.URL.Path)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Request ended", err.Error(), r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Lifecycle error", err.Error(), r.URL.Path)
	}
}

// IsLifecycleRefusal reports whether err is one of the machine's own
// refusals, which WriteLifecycleProblem maps.
func IsLifecycleRefusal(err error) bool {
	return errors.Is(err, repos.ErrIndexing) || errors.Is(err, repos.ErrNotOpen) ||
		errors.Is(err, repos.ErrClosed) || errors.Is(err, repos.ErrUnavailable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
