// Package synthesize — Clustering for the prune step: gathers facts from git
// and groups them by semantic similarity using precomputed cosine distances
// (SQLite vec0) + HDBSCAN.
package synthesize

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"knomit/internal/cluster"
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
			Domain:     fact.Domain,
			Entities:   fact.Entities,
			Confidence: fact.Confidence,
			Sources:    fact.Sources,
		})
	}
	return facts, nil
}

// PairwiseDistancer computes cosine distance matrices in the index (via SQLite vec0).
type PairwiseDistancer interface {
	PairwiseDistances(paths []string) (retPaths []string, dist [][]float64, err error)
}

// clusterFactsForPrune groups facts by semantic similarity using embeddings.
// Returns a slice of fact groups — each group is a cluster to be reviewed together.
// When embeddings are unavailable or too few, returns all facts as a single group.
//
// Cosine distances are computed via SQLite's vec_distance_cosine (delegating to
// the index), then HDBSCAN runs on the precomputed distance matrix. This avoids
// loading 768-dim embedding vectors into Go and sidesteps the curse of
// dimensionality that makes Euclidean HDBSCAN fail on high-dim data.
func clusterFactsForPrune(facts []factForLLM, idx SearchIndex, embedder Embedder, step RecipeStep, onProgress func(ProgressEvent)) ([][]factForLLM, error) {
	minCluster := step.MinClusterSize
	if minCluster == 0 {
		minCluster = 3
	}

	pd, hasPD := idx.(PairwiseDistancer)
	if embedder == nil || !hasPD {
		log.Debug().Msg("prune: no embeddings available, using single group")
		onProgress(ProgressEvent{Phase: "cluster", Message: "no embeddings, reviewing all facts"})
		return [][]factForLLM{facts}, nil
	}

	// Collect all fact paths.
	allPaths := make([]string, len(facts))
	for i, f := range facts {
		allPaths[i] = f.File
	}

	// Compute pairwise cosine distances in SQLite.
	onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("computing distances for %d facts", len(facts))})
	retPaths, dist, err := pd.PairwiseDistances(allPaths)
	if err != nil {
		log.Warn().Err(err).Msg("prune: pairwise distances failed, using single group")
		onProgress(ProgressEvent{Phase: "cluster", Message: "distance computation failed, reviewing all facts"})
		return [][]factForLLM{facts}, nil
	}

	if len(retPaths) < minCluster {
		log.Debug().Int("with_embedding", len(retPaths)).Int("min_cluster", minCluster).Msg("prune: insufficient embeddings, using single group")
		onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("insufficient embeddings (%d), reviewing all facts", len(retPaths))})
		return [][]factForLLM{facts}, nil
	}

	// Build path→fact index for mapping results back.
	factByPath := map[string]factForLLM{}
	for _, f := range facts {
		factByPath[f.File] = f
	}

	labels := cluster.HDBSCANPrecomputed(dist, cluster.HDBSCANOptions{
		MinClusterSize: minCluster,
	})

	// Group facts by cluster label. Noise (label -1) is skipped — singletons
	// have no peers to compare against for prune/merge.
	clusterMap := map[int][]factForLLM{}
	noiseCount := 0
	for i, path := range retPaths {
		label := labels[i]
		if label == -1 {
			noiseCount++
			continue
		}
		clusterMap[label] = append(clusterMap[label], factByPath[path])
	}

	log.Debug().Int("clusters", len(clusterMap)).Int("noise", noiseCount).Int("total", len(facts)).Msg("prune: clustering complete")
	onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("%d clusters (%d noise skipped)", len(clusterMap), noiseCount)})

	if len(clusterMap) == 0 {
		log.Debug().Msg("prune: no clusters formed, using single group")
		onProgress(ProgressEvent{Phase: "cluster", Message: "no clusters formed, reviewing all facts"})
		return [][]factForLLM{facts}, nil
	}

	groups := make([][]factForLLM, 0, len(clusterMap))
	for _, group := range clusterMap {
		groups = append(groups, group)
	}
	return groups, nil
}
