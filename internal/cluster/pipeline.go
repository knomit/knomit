// End-to-end clustering pipeline: float32->float64 conversion -> UMAP
// dimensionality reduction -> HDBSCAN density clustering -> FCA metadata
// splitting.
package cluster

import "fmt"

// ClusterOptions configures the ClusterFacts pipeline.
type ClusterOptions struct {
	UMAPDimensions int // target dimensions for UMAP (default 5)
	MinClusterSize int // minimum points per cluster (default 3)
}

// ClusterResult holds the output of ClusterFacts.
type ClusterResult struct {
	Clusters map[int][]int // cluster label -> fact indices
	Noise    []int         // fact indices classified as noise
}

// ClusterFacts clusters fact embeddings using UMAP + HDBSCAN + FCA metadata split.
// embeddings[i] is the embedding for facts[i] (or metas[i]).
//
// Pipeline steps:
//  1. float32 -> float64 conversion: HDBSCAN and UMAP operate on float64 for
//     numerical precision; embedding models typically produce float32.
//  2. UMAP dimensionality reduction with nNeighbors=15 (clamped to [1, n-1] so
//     it never exceeds the dataset size). Seed=42 for reproducibility;
//     minDist=0.1 is the standard UMAP default for preserving local structure.
//  3. HDBSCAN density clustering on the reduced vectors.
//  4. FCA metadata split to break clusters by shared domain/entity tags.
func ClusterFacts(embeddings [][]float32, metas []FactMeta, opts ClusterOptions) (ClusterResult, error) {
	n := len(embeddings)
	if n == 0 {
		return ClusterResult{Clusters: map[int][]int{}, Noise: nil}, nil
	}
	if len(metas) != n {
		return ClusterResult{}, fmt.Errorf("cluster: embeddings and metas length mismatch: %d vs %d", n, len(metas))
	}

	if opts.UMAPDimensions <= 0 {
		opts.UMAPDimensions = 5
	}
	if opts.MinClusterSize <= 0 {
		opts.MinClusterSize = 3
	}

	// 1. float32 -> float64
	vecs := make([][]float64, n)
	for i, emb := range embeddings {
		v := make([]float64, len(emb))
		for j, x := range emb {
			v[j] = float64(x)
		}
		vecs[i] = v
	}

	// 2. UMAP dimensionality reduction
	// nNeighbors is clamped to [1, n-1] so it never exceeds the dataset size.
	nNeighbors := 15
	if nNeighbors >= n {
		nNeighbors = n - 1
	}
	if nNeighbors < 1 {
		nNeighbors = 1
	}
	reduced, err := UMAP(vecs, UMAPOptions{
		NComponents: opts.UMAPDimensions,
		NNeighbors:  nNeighbors,
		MinDist:     0.1,  // standard UMAP default for local structure preservation
		Seed:        42,   // fixed seed for reproducible results
	})
	if err != nil {
		return ClusterResult{}, fmt.Errorf("cluster: UMAP failed: %w", err)
	}

	// 3. HDBSCAN
	hdbscanLabels := HDBSCAN(reduced, HDBSCANOptions{
		MinClusterSize: opts.MinClusterSize,
	})

	// 4. FCA metadata split
	finalLabels := SplitByMetadata(metas, hdbscanLabels, opts.MinClusterSize)

	// 5. Build ClusterResult
	clusters := make(map[int][]int)
	noise := []int{}
	for i, l := range finalLabels {
		if l == -1 {
			noise = append(noise, i)
		} else {
			clusters[l] = append(clusters[l], i)
		}
	}

	return ClusterResult{Clusters: clusters, Noise: noise}, nil
}
