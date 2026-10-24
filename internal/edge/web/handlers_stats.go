package web

import (
	"context"
	"knomit/internal/edge/web/httpx"
	"net/http"

	"knomit/internal/engine/store/index"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// loadStats reads one repo's aggregate stats. A store that cannot be reached
// is WithRead's error, returned as-is (httpx.WriteStoreError answers it 503).
func loadStats(ctx context.Context, ri *repos.RepoInstance, branch, pathPrefix, axis string) (index.StatsResult, error) {
	var (
		result index.StatsResult
		err    error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		result, err = svc.FactQuery().Stats(ctx, branch, pathPrefix, axis)
	}); aerr != nil {
		return result, aerr
	}
	return result, err
}

// loadHighlights is the lens union's axis-correction path: only that handler
// needs to re-cut one mount's top-N by an axis the mount did not pick, and it
// needs to do it without paying for the aggregates it already holds.
func loadHighlights(ctx context.Context, ri *repos.RepoInstance, branch, pathPrefix, axis string) ([]index.Highlight, bool, error) {
	var (
		out      []index.Highlight
		fallback bool
		err      error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		out, fallback, err = svc.FactQuery().Highlights(ctx, branch, pathPrefix, axis)
	}); aerr != nil {
		return out, fallback, aerr
	}
	return out, fallback, err
}

// statsView is the HAL response body for the stats endpoint.
type statsView struct {
	Total         int               `json:"total"`
	AvgConfidence float64           `json:"avg_confidence"`
	Domains       map[string]int    `json:"domains"`
	Entities      map[string]int    `json:"entities"`
	Types         map[string]int    `json:"types"`
	Highlights    []index.Highlight `json:"highlights"`
	DefaultAxis   string            `json:"default_axis"`
	Links         hal.LinkMap       `json:"_links"`
}

// handleHALStats serves GET /repos/{repo}/branches/{branch}/stats.
func handleHALStats(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := httpx.BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		pathPrefix := r.URL.Query().Get("path")
		axisParam := r.URL.Query().Get("axis")
		result, err := loadStats(r.Context(), ri, branch, pathPrefix, axisParam)
		if err != nil {
			httpx.WriteStoreError(w, r, err, "Failed to load stats", branch)
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
			highlights = []index.Highlight{}
		}
		axis := result.DefaultAxis
		if axis == "" {
			axis = index.AxisConfidence
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
