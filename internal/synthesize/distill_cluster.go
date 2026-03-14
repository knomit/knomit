// Package synthesize — Clustering for the distill step: two strategies
// depending on RAPTOR depth. Depth 0 uses Louvain on the persisted graph;
// depth > 0 falls back to single-group (in-memory facts lack graph edges).
package synthesize

import (
	"fmt"

	"github.com/rs/zerolog/log"
)

// workFact is a fact with an optional in-memory embedding (used during RAPTOR recursion).
type workFact struct {
	factForLLM
	embedding []float32
}

// distillClusterFromIndex clusters facts using Louvain community detection.
// Note: Louvain operates on the persisted graph (SIMILAR_TO + TAGGED + IN_DOMAIN edges).
// It does not need an embedder — SIMILAR_TO edges are built at sync/learn time.
func distillClusterFromIndex(facts []workFact, idx SearchIndex, resolution float64, minCommunitySize int, onProgress func(ProgressEvent)) (map[int][]factForLLM, error) {
	onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("running Louvain (resolution=%.2f)", resolution)})

	result, err := idx.ClusterFacts(resolution, minCommunitySize)
	if err != nil {
		log.Warn().Err(err).Msg("distill: Louvain failed")
		return nil, nil
	}

	factByPath := map[string]factForLLM{}
	for _, f := range facts {
		factByPath[f.File] = f.factForLLM
	}

	clusterMap := map[int][]factForLLM{}
	for id, paths := range result.Clusters {
		for _, p := range paths {
			if f, ok := factByPath[p]; ok {
				clusterMap[id] = append(clusterMap[id], f)
			}
		}
	}

	return clusterMap, nil
}

// distillClusterInMemory clusters facts using in-memory embeddings (RAPTOR depth > 0).
// With Louvain, depth > 0 falls back to single-group since graph edges only exist
// for indexed facts.
func distillClusterInMemory(facts []workFact, resolution float64, onProgress func(ProgressEvent)) map[int][]factForLLM {
	onProgress(ProgressEvent{Phase: "cluster", Message: "depth > 0: single group (in-memory facts)"})
	return nil
}
