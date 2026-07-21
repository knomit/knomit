package web

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// activityProvider is the narrow interface the activity handler depends on.
type activityProvider interface {
	Activity(ctx context.Context, ri *repos.RepoInstance, branch, path string) (store.ActivityResult, error)
}

// defaultActivityProvider implements activityProvider using the store.
type defaultActivityProvider struct{}

func (defaultActivityProvider) Activity(ctx context.Context, ri *repos.RepoInstance, branch, path string) (store.ActivityResult, error) {
	var (
		result store.ActivityResult
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		result, err = svc.Search().Activity(ctx, branch, path)
	})
	return result, err
}

// activityView is the HAL response body for the activity endpoint.
type activityView struct {
	LastCommit string      `json:"last_commit"`
	Total      int         `json:"total"`
	Changes7d  int         `json:"changes_7d"`
	Changes30d int         `json:"changes_30d"`
	Changes90d int         `json:"changes_90d"`
	Links      hal.LinkMap `json:"_links"`
}

// handleHALActivity serves GET /repos/{repo}/branches/{branch}/activity.
func handleHALActivity(b hal.URLBuilder, m *repos.Manager, provider activityProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}
		path := r.URL.Query().Get("path")

		result, err := provider.Activity(r.Context(), ri, branch, path)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load activity", branch)
			return
		}

		selfURL := b.Branch(repoName, a) + "/activity"
		if r.URL.RawQuery != "" {
			selfURL += "?" + r.URL.RawQuery
		}

		view := activityView{
			LastCommit: result.LastCommit,
			Total:      result.Total,
			Changes7d:  result.Changes7d,
			Changes30d: result.Changes30d,
			Changes90d: result.Changes90d,
			Links:      hal.LinkMap{"self": {Href: selfURL}},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
