package web

import (
	"context"
	"net/http"

	"knomit/internal/engine/store/index"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// loadActivity reads one repo's commit activity. A repo whose store is not
// live yields the zero ActivityResult and a nil error.
func loadActivity(ctx context.Context, ri *repos.RepoInstance, branch, path string) (index.ActivityResult, error) {
	var (
		result index.ActivityResult
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		result, err = svc.HistoryQuery().Activity(ctx, branch, path)
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
func handleHALActivity(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}
		path := r.URL.Query().Get("path")

		result, err := loadActivity(r.Context(), ri, branch, path)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load activity", branch)
			return
		}

		selfURL := selfWithQuery(b.Branch(repoName, a)+"/activity", r)

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
