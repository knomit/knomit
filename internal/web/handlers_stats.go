package web

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// statsProvider is the narrow interface the stats handler depends on.
//
// Highlights is the lens union's axis-correction path: only that handler needs
// to re-cut one mount's top-N by an axis the mount did not pick, and it needs
// to do it without paying for the aggregates it already holds. It sits here
// rather than in its own interface because it is the same store surface and
// the same fake in tests.
type statsProvider interface {
	Stats(ctx context.Context, ri *repos.RepoInstance, branch, pathPrefix, axis string) (store.StatsResult, error)
	Highlights(ctx context.Context, ri *repos.RepoInstance, branch, pathPrefix, axis string) ([]store.Highlight, bool, error)
}

// defaultStatsProvider is the production statsProvider.
type defaultStatsProvider struct{}

func (defaultStatsProvider) Stats(ctx context.Context, ri *repos.RepoInstance, branch, pathPrefix, axis string) (store.StatsResult, error) {
	var (
		result store.StatsResult
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		result, err = svc.FactQuery().Stats(ctx, branch, pathPrefix, axis)
	})
	return result, err
}

func (defaultStatsProvider) Highlights(ctx context.Context, ri *repos.RepoInstance, branch, pathPrefix, axis string) ([]store.Highlight, bool, error) {
	var (
		out      []store.Highlight
		fallback bool
		err      error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, fallback, err = svc.FactQuery().Highlights(ctx, branch, pathPrefix, axis)
	})
	return out, fallback, err
}

// statsView is the HAL response body for the stats endpoint.
type statsView struct {
	Total         int               `json:"total"`
	AvgConfidence float64           `json:"avg_confidence"`
	Domains       map[string]int    `json:"domains"`
	Entities      map[string]int    `json:"entities"`
	Types         map[string]int    `json:"types"`
	Highlights    []store.Highlight `json:"highlights"`
	DefaultAxis   string            `json:"default_axis"`
	Links         hal.LinkMap       `json:"_links"`
}

// handleHALStats serves GET /repos/{repo}/branches/{branch}/stats.
func handleHALStats(b hal.URLBuilder, provider statsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		pathPrefix := r.URL.Query().Get("path")
		axisParam := r.URL.Query().Get("axis")
		result, err := provider.Stats(r.Context(), ri, branch, pathPrefix, axisParam)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load stats", branch)
			return
		}

		selfURL := b.Branch(repoName, a) + "/stats"

		domains := result.Domains
		if domains == nil {
			domains = map[string]int{}
		}
		entities := result.Entities
		if entities == nil {
			entities = map[string]int{}
		}
		types := result.Types
		if types == nil {
			types = map[string]int{}
		}
		highlights := result.Highlights
		if highlights == nil {
			highlights = []store.Highlight{}
		}
		axis := result.DefaultAxis
		if axis == "" {
			axis = store.AxisConfidence
		}

		view := statsView{
			Total:         result.Total,
			AvgConfidence: result.AvgConfidence,
			Domains:       domains,
			Entities:      entities,
			Types:         types,
			Highlights:    highlights,
			DefaultAxis:   axis,
			Links:         hal.LinkMap{"self": {Href: selfURL}},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
