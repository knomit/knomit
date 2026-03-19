// Package synthesize — Distill (RAPTOR) step of the synthesis pipeline:
// iteratively clusters facts and uses an LLM to synthesize higher-order
// insights, repeating for multiple depth levels.
//
// RAPTOR (Recursive Abstractive Processing for Tree-Organized Retrieval):
// at each depth, cluster facts -> synthesize patterns -> re-embed for next level.
package synthesize

import (
	"context"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/llm"
	"knomit/internal/store"
)

// executeDistillStep runs one distill (RAPTOR) step of the synthesis pipeline.
func executeDistillStep(ctx context.Context, gs GitStore, idx SearchIndex, embedder Embedder, adapter llm.LLMAdapter, step RecipeStep, recipe Recipe, profile Profile, onProgress func(ProgressEvent)) error {
	maxDepth := step.MaxDepth
	if maxDepth == 0 {
		maxDepth = 1
	}
	resolution := step.Resolution
	if resolution <= 0 {
		resolution = 1.0
	}

	log.Debug().Int("max_depth", maxDepth).Float64("resolution", resolution).Msg("distill: config")

	// Gather initial facts from the index (all facts, no filter).
	searchResults, err := idx.Search(store.SearchQuery{Limit: 100_000})
	if err != nil {
		return fmt.Errorf("distill: search all: %w", err)
	}
	log.Debug().Int("facts", len(searchResults)).Msg("distill: gathered facts from index")
	if len(searchResults) == 0 {
		onProgress(ProgressEvent{Phase: "distill", Message: "no facts in index"})
		return nil
	}

	// Build current working set from search results.
	currentFacts := make([]workFact, 0, len(searchResults))
	for _, r := range searchResults {
		currentFacts = append(currentFacts, workFact{
			factForLLM: factForLLM{
				File:       r.Path,
				Title:      r.Title,
				Body:       r.Body,
				Type:       r.Type,
				Domain:     r.Domain,
				Entities:   r.Entities,
				Confidence: r.Confidence,
				Sources:    r.Sources,
			},
		})
	}

	var allSynthesized []distillFact
	allForget := map[string]bool{}

	for depth := 0; depth < maxDepth; depth++ {
		log.Debug().Int("depth", depth+1).Int("max_depth", maxDepth).Int("facts", len(currentFacts)).Msg("distill: RAPTOR depth")
		onProgress(ProgressEvent{Phase: "raptor-depth", Message: fmt.Sprintf("%d/%d", depth+1, maxDepth)})

		var clusterMap map[int][]factForLLM

		if depth == 0 {
			// Initial depth uses Louvain on the persisted graph.
			clusterMap, err = distillClusterFromIndex(currentFacts, idx, resolution, 2, onProgress)
		} else {
			// Subsequent depths: in-memory facts have no graph edges, fall back to single group.
			clusterMap = distillClusterInMemory(currentFacts, resolution, onProgress)
		}
		if err != nil {
			return err
		}

		if clusterMap == nil {
			// Fallback: send all facts as one cluster.
			group := make([]factForLLM, len(currentFacts))
			for i, f := range currentFacts {
				group[i] = f.factForLLM
			}
			synthesized, forget, err := runDistillOnGroup(ctx, gs, idx, adapter, group, step, recipe, profile, onProgress)
			if err != nil {
				return err
			}
			allSynthesized = append(allSynthesized, synthesized...)
			for _, p := range forget {
				allForget[p] = true
			}
			break
		}

		onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("%d clusters", len(clusterMap))})

		if len(clusterMap) == 0 {
			break
		}

		var depthSynthesized []distillFact
		if ba, ok := adapter.(llm.BatchAdapter); ok && ba.BatchEnabled() {
			// Batch all groups for this depth at once.
			var allGroupFacts []factForLLM
			for _, group := range clusterMap {
				allGroupFacts = append(allGroupFacts, group...)
			}
			synthesized, forget, err := distillBatch(ctx, ba, allGroupFacts, step, recipe, profile, onProgress)
			if err != nil {
				return err
			}
			depthSynthesized = append(depthSynthesized, synthesized...)
			for _, p := range forget {
				allForget[p] = true
			}
		} else {
			for _, group := range clusterMap {
				synthesized, forget, err := runDistillOnGroup(ctx, gs, idx, adapter, group, step, recipe, profile, onProgress)
				if err != nil {
					return err
				}
				depthSynthesized = append(depthSynthesized, synthesized...)
				for _, p := range forget {
					allForget[p] = true
				}
			}
		}
		allSynthesized = append(allSynthesized, depthSynthesized...)

		// RAPTOR recursion: re-embed new synthesized facts for next depth.
		if depth+1 < maxDepth && len(depthSynthesized) > 0 && embedder != nil {
			nextFacts := make([]workFact, 0, len(depthSynthesized))
			for _, df := range depthSynthesized {
				embText := fmt.Sprintf("%s %s %s %s",
					df.Title, df.Body,
					strings.Join(df.Entities, " "),
					strings.Join(df.Domain, " "),
				)
				vec, err := embedder.Embed(embText)
				if err != nil {
					vec = nil
				}
				nextFacts = append(nextFacts, workFact{
					factForLLM: factForLLM{
						File:       df.Path,
						Title:      df.Title,
						Body:       df.Body,
						Type:       df.Type,
						Domain:     df.Domain,
						Entities:   df.Entities,
						Confidence: df.Confidence,
						Sources:    1,
					},
					embedding: vec,
				})
			}
			currentFacts = nextFacts
		} else {
			break
		}
	}

	// Convert allForget map to slice for ApplyDistillDecisions.
	retractPaths := make([]string, 0, len(allForget))
	for p := range allForget {
		retractPaths = append(retractPaths, p)
	}

	stats, _, err := ApplyDistillDecisions(gs, idx, allSynthesized, retractPaths, recipe.Name, onProgress)
	if err != nil {
		return err
	}

	onProgress(ProgressEvent{Phase: "distill-done", Message: fmt.Sprintf("%d synthesized, %d forgotten", stats.Synthesized, stats.Pruned)})
	return nil
}
