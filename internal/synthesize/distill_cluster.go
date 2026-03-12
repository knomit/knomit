// Package synthesize — Clustering for the distill step: two strategies
// depending on RAPTOR depth. Depth 0 uses SQLite-stored embeddings via
// PairwiseDistances; depth > 0 uses in-memory cosine HDBSCAN on freshly
// computed embeddings.
package synthesize

import (
	"fmt"

	"github.com/rs/zerolog/log"
	"knomit/internal/cluster"
)

// workFact is a fact with an optional in-memory embedding (used during RAPTOR recursion).
type workFact struct {
	factForLLM
	embedding []float32
}

// distillClusterFromIndex clusters facts whose embeddings are stored in the index,
// using PairwiseDistances (SQLite vec_distance_cosine) + HDBSCANPrecomputed.
// Returns nil clusterMap if embeddings are unavailable (caller should fall back).
func distillClusterFromIndex(facts []workFact, idx SearchIndex, embedder Embedder, minCluster int, onProgress func(ProgressEvent)) (map[int][]factForLLM, error) {
	pd, hasPD := idx.(PairwiseDistancer)
	if embedder == nil || !hasPD {
		onProgress(ProgressEvent{Phase: "cluster", Message: "no embeddings, using single cluster"})
		return nil, nil
	}

	paths := make([]string, len(facts))
	for i, f := range facts {
		paths[i] = f.File
	}

	onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("computing distances for %d facts", len(facts))})
	retPaths, dist, err := pd.PairwiseDistances(paths)
	if err != nil {
		log.Warn().Err(err).Msg("distill: pairwise distances failed")
		return nil, nil
	}
	if len(retPaths) < minCluster {
		onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("insufficient embeddings (%d)", len(retPaths))})
		return nil, nil
	}

	factByPath := map[string]factForLLM{}
	for _, f := range facts {
		factByPath[f.File] = f.factForLLM
	}

	labels := cluster.HDBSCANPrecomputed(dist, cluster.HDBSCANOptions{
		MinClusterSize: minCluster,
	})

	clusterMap := map[int][]factForLLM{}
	for i, path := range retPaths {
		if labels[i] == -1 {
			continue
		}
		clusterMap[labels[i]] = append(clusterMap[labels[i]], factByPath[path])
	}
	return clusterMap, nil
}

// distillClusterInMemory clusters facts using in-memory embeddings (for RAPTOR depth > 0
// where embeddings are freshly computed and not stored in the index).
// Uses HDBSCAN with cosine distance directly. Returns nil if insufficient embeddings.
func distillClusterInMemory(facts []workFact, minCluster int, onProgress func(ProgressEvent)) map[int][]factForLLM {
	var withEmbedding []int
	for i, f := range facts {
		if len(f.embedding) > 0 {
			withEmbedding = append(withEmbedding, i)
		}
	}
	if len(withEmbedding) < minCluster {
		onProgress(ProgressEvent{Phase: "cluster", Message: fmt.Sprintf("insufficient embeddings (%d)", len(withEmbedding))})
		return nil
	}

	// Convert float32→float64: HDBSCAN operates on float64 for numerical precision.
	vecs := make([][]float64, len(withEmbedding))
	for i, idx := range withEmbedding {
		v32 := facts[idx].embedding
		v64 := make([]float64, len(v32))
		for j, x := range v32 {
			v64[j] = float64(x)
		}
		vecs[i] = v64
	}

	labels := cluster.HDBSCAN(vecs, cluster.HDBSCANOptions{
		MinClusterSize: minCluster,
		Distance:       cluster.CosineDistance,
	})

	clusterMap := map[int][]factForLLM{}
	for i, fi := range withEmbedding {
		if labels[i] == -1 {
			continue
		}
		clusterMap[labels[i]] = append(clusterMap[labels[i]], facts[fi].factForLLM)
	}
	return clusterMap
}
