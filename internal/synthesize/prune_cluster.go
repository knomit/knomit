// Package synthesize — Clustering for the prune step: gathers facts from git
// and groups them by Louvain community detection (graph-based).
package synthesize

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/mcp"
)

// gatherAllFacts reads all .md facts from git and returns them as factForLLM slices.
func gatherAllFacts(gs GitStore) ([]factForLLM, error) {
	paths, err := gs.ListAll()
	if err != nil {
		return nil, fmt.Errorf("gatherAllFacts: list: %w", err)
	}

	facts := make([]factForLLM, 0, len(paths))
	for _, path := range paths {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		content, err := gs.ReadFile(path)
		if err != nil {
			continue // skip unreadable files
		}
		fact, err := mcp.ParseFact(path, content)
		if err != nil {
			continue // skip non-fact files
		}
		facts = append(facts, factForLLM{
			File:       fact.Path,
			Title:      fact.Title,
			Body:       fact.Body,
			Type:       string(fact.Type),
			Domain:     fact.Domain,
			Entities:   fact.Entities,
			Confidence: fact.Confidence,
			Sources:    fact.Sources,
		})
	}
	return facts, nil
}

// clusterFactsForPrune groups facts by Louvain community detection.
func clusterFactsForPrune(facts []factForLLM, idx SearchIndex, embedder Embedder, step RecipeStep, onProgress func(ProgressEvent)) ([][]factForLLM, error) {
	resolution := step.Resolution
	if resolution <= 0 {
		resolution = 1.0
	}

	onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("running Louvain (resolution=%.2f) on %d facts", resolution, len(facts))})

	result, err := idx.ClusterFacts(resolution, 2)
	if err != nil {
		log.Warn().Err(err).Msg("prune: Louvain failed, using single group")
		onProgress(ProgressEvent{Phase: "cluster", Message: "clustering failed, reviewing all facts"})
		return [][]factForLLM{facts}, nil
	}

	factByPath := map[string]factForLLM{}
	for _, f := range facts {
		factByPath[f.File] = f
	}

	groups := make([][]factForLLM, 0, len(result.Clusters))
	for _, paths := range result.Clusters {
		var group []factForLLM
		for _, p := range paths {
			if f, ok := factByPath[p]; ok {
				group = append(group, f)
			}
		}
		if len(group) > 0 {
			groups = append(groups, group)
		}
	}

	log.Debug().Int("clusters", len(groups)).Int("noise", len(result.Noise)).Int("total", len(facts)).Msg("prune: clustering complete")
	onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("%d clusters (%d noise skipped)", len(groups), len(result.Noise))})

	if len(groups) == 0 {
		onProgress(ProgressEvent{Phase: "cluster", Message: "no clusters formed, reviewing all facts"})
		return [][]factForLLM{facts}, nil
	}

	return groups, nil
}
