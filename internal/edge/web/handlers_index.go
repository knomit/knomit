package web

import (
	"knomit/internal/edge/web/httpx"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

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
			httpx.WriteLifecycleProblem(w, r, b, name, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, summaryFor(b, name, ri))
	}
}
