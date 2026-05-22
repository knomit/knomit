package web

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/detect"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// ScorerLike is the small surface of *detect.Scorer that the handler
// needs. Declared as an interface so tests can substitute a stub without
// constructing a real Scorer (which requires an embedder).
type ScorerLike interface {
	ScoreBlocks(blocks []detect.Block, intents []string) []detect.BlockResult
	ScoreBlocksWithNovelty(blocks []detect.Block, intents []string, searcher detect.FactSearcher) []detect.BlockResult
}

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
func handleDetect(scorers map[string]ScorerLike, mgr *repos.Manager) http.HandlerFunc {
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

// buildFactSearcher is a stub for this task. Task 9 wires it to the real store.
// Returning nil here causes ScoreBlocksWithNovelty to gracefully fall back to
// intent-only scoring (which is honest about not having novelty data yet).
func buildFactSearcher(_ *repos.Manager, _, _ string) detect.FactSearcher {
	return nil
}
