package web

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// completionsProvider is the narrow interface the completions handler depends on.
type completionsProvider interface {
	Completions(ctx context.Context, ri *repos.RepoInstance, branch, category, prefix string, limit int) ([]string, error)
}

// defaultCompletionsProvider implements completionsProvider using the store.
type defaultCompletionsProvider struct{}

func (defaultCompletionsProvider) Completions(ctx context.Context, ri *repos.RepoInstance, branch, category, prefix string, limit int) ([]string, error) {
	var (
		out []string
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.FactQuery().Completions(ctx, branch, category, prefix, limit)
	})
	return out, err
}

// completionsView is the HAL response body for the completions endpoint.
type completionsView struct {
	Values []string    `json:"values"`
	Links  hal.LinkMap `json:"_links"`
}

// handleHALCompletions serves GET /repos/{repo}/branches/{branch}/completions.
func handleHALCompletions(b hal.URLBuilder, provider completionsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		category := r.URL.Query().Get("category")
		prefix := r.URL.Query().Get("prefix")

		values, err := provider.Completions(r.Context(), ri, branch, category, prefix, 20)
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
