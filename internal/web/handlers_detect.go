package web

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/detect"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

type detectRequest struct {
	Blocks         []detect.Block      `json:"blocks"`
	Intents        []string            `json:"intents"`
	NoveltyContext *noveltyContextSpec `json:"novelty_context,omitempty"`
}

type noveltyContextSpec struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

type detectResponse struct {
	Blocks []detect.BlockResult `json:"blocks"`
}

// handleDetect returns the HTTP handler for POST /profiles/{profile}/detect.
// scorers is keyed by profile name; unknown profiles produce 404.
// mgr is optional; if provided and the request includes novelty_context, a
// FactSearcher is built from the named repo/branch for novelty scoring.
func handleDetect(scorers map[string]detect.BlockScorer, mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		profile := chi.URLParam(r, "profile")
		scorer, ok := scorers[profile]
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Not Found",
				"no detect support for profile "+profile, r.URL.Path)
			return
		}

		var req detectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Bad Request",
				"invalid JSON body: "+err.Error(), r.URL.Path)
			return
		}

		var results []detect.BlockResult
		if req.NoveltyContext != nil {
			var searcher detect.FactSearcher
			if mgr != nil {
				searcher = buildFactSearcher(mgr, req.NoveltyContext.Repo, req.NoveltyContext.Branch)
			}
			results = scorer.ScoreBlocksWithNovelty(req.Blocks, req.Intents, searcher)
		} else {
			results = scorer.ScoreBlocks(req.Blocks, req.Intents)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(detectResponse{Blocks: results})
	}
}

// buildFactSearcher returns a FactSearcher backed by the named repo/branch in
// mgr. Returns nil if the repo is not registered (ScoreBlocksWithNovelty
// gracefully falls back to intent-only scoring in that case).
func buildFactSearcher(mgr *repos.Manager, repoName, branch string) detect.FactSearcher {
	ri := mgr.Get(repoName)
	if ri == nil {
		return nil
	}
	return &repoFactSearcher{ri: ri, branch: branch}
}

// repoFactSearcher implements detect.FactSearcher using the store's vector
// similarity search (SearchIndex.Search with QueryVec).
type repoFactSearcher struct {
	ri     *repos.RepoInstance
	branch string
}

// NearestFacts queries the store's vec0 KNN index for the k facts closest to
// vec on the configured branch. Score is in [0, 100] from the store; we
// normalise to [0, 1] for the detect layer.
func (r *repoFactSearcher) NearestFacts(vec []float32, k int) ([]detect.SimilarFact, error) {
	var out []detect.SimilarFact
	var searchErr error
	r.ri.WithRead(func(svc *store.Service) {
		results, err := svc.Search().Search(context.Background(), r.branch, store.SearchOptions{
			QueryVec: vec,
			Limit:    k,
		})
		if err != nil {
			searchErr = err
			return
		}
		out = make([]detect.SimilarFact, 0, len(results))
		for _, res := range results {
			out = append(out, detect.SimilarFact{
				Path:       res.Path,
				Similarity: res.Score / 100.0,
			})
		}
	})
	return out, searchErr
}
