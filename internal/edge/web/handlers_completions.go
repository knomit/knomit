package web

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// readCompletions reads the completion values for one repo and branch. A store
// that cannot be reached is WithRead's error, returned as-is (writeStoreError
// answers it 503).
func readCompletions(ctx context.Context, ri *repos.RepoInstance, branch, category, prefix string, limit int) ([]string, error) {
	var (
		out []string
		err error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		out, err = svc.FactQuery().Completions(ctx, branch, category, prefix, limit)
	}); aerr != nil {
		return out, aerr
	}
	return out, err
}

// completionsView is the HAL response body for the completions endpoint.
type completionsView struct {
	Values []string    `json:"values"`
	Links  hal.LinkMap `json:"_links"`
}

// handleHALCompletions serves GET /repos/{repo}/branches/{branch}/completions.
func handleHALCompletions(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		category := r.URL.Query().Get("category")
		prefix := r.URL.Query().Get("prefix")

		values, err := readCompletions(r.Context(), ri, branch, category, prefix, 20)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load completions", branch)
			return
		}
		if values == nil {
			values = []string{}
		}

		selfURL := selfWithQuery(b.Branch(repoName, a)+"/completions", r)

		view := completionsView{
			Values: values,
			Links:  hal.LinkMap{"self": {Href: selfURL}},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
