package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// cancelIndexURL is the cancel-index action for a repo: a POST on a
// colon-suffixed sub-resource, the same spelling as /repo-creates/{id}:cancel
// (kb/decisions/repos/create-job/cancel-is-a-post-action).
func cancelIndexURL(b hal.URLBuilder, repo string) string {
	return b.Repo(repo) + "/index:cancel"
}

// handleCancelIndex serves POST /api/v1/repos/{repo}/index:cancel.
//
// It sends CancelIndex: the index job is cancelled (it lands after its
// in-flight embedding batch) and drained, the result becomes
// index_state "error" with index_reason "indexing cancelled", and the repo
// stays usable — Attach, Detach, Swap and Rebuild are accepted again. With no
// job running it is a no-op. The reply is the repo's status after the drain.
func handleCancelIndex(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		if _, err := m.Send(r.Context(), ri, repos.CancelIndex()); err != nil {
			writeLifecycleProblem(w, r, b, name, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, summaryFor(b, name, ri))
	}
}

// writeLifecycleProblem maps a lifecycle machine's refusal onto HTTP.
//
//   - ErrIndexing → 409 "Repo is indexing", with a cancel-index link: the
//     caller waits, or cancels indexing and retries.
//   - ErrNotOpen → 503: the store is not attached (a repo still being created,
//     or one whose store is being reopened). Transient.
//   - ErrClosed / ErrUnavailable → 503: the repo is going away or could not be
//     built; the repo row says why.
//   - the request's own cancellation → 503 as well: nothing was decided.
//
// Anything else is the event's own error, and the caller maps it first.
func writeLifecycleProblem(w http.ResponseWriter, r *http.Request, b hal.URLBuilder, repo string, err error) {
	switch {
	case errors.Is(err, repos.ErrIndexing):
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Repo is indexing",
			"wait for it to finish or cancel indexing", r.URL.Path,
			map[string]any{"_links": map[string]any{
				"cancel-index": map[string]string{"href": cancelIndexURL(b, repo), "method": http.MethodPost},
			}})
	case errors.Is(err, repos.ErrNotOpen):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo not open", err.Error()+"; try again", r.URL.Path)
	case errors.Is(err, repos.ErrClosed), errors.Is(err, repos.ErrUnavailable):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Repo unavailable", err.Error(), r.URL.Path)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Request ended", err.Error(), r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Lifecycle error", err.Error(), r.URL.Path)
	}
}

// isLifecycleRefusal reports whether err is one of the machine's own
// refusals, which writeLifecycleProblem maps.
func isLifecycleRefusal(err error) bool {
	return errors.Is(err, repos.ErrIndexing) || errors.Is(err, repos.ErrNotOpen) ||
		errors.Is(err, repos.ErrClosed) || errors.Is(err, repos.ErrUnavailable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
