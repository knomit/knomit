package synthesize

import "sort"

// ClusterResult holds a community partition over fact paths. It is the currency
// the bridge engine consumes: enumerateBridgeCandidates and buildScoredBridges
// both derive their path→community map from it (via bridgePathCommunities) so
// they always operate on the SAME assignment.
//
// Historically this lived in the store layer and was produced by a graph-wide
// Louvain pass (store.searchIndex.ClusterFacts). That global clustering was
// removed when clustering moved in-process and scoped (ScopedCluster over
// idx.SubgraphEdges); ClusterResult now lives here and is built from
// ScopedCluster output via clusterResultFromGroups.
type ClusterResult struct {
	Clusters map[int][]string // community ID → fact paths
	Noise    []string         // fact paths in communities below minCommunitySize
}

// ClusterOf returns a path→community-id map for every path in the result.
// Clustered paths map to their community id (≥ 0). Noise paths each receive a
// distinct negative id (−1, −2, …) assigned in sorted-path order so the
// mapping is deterministic across runs. Paths absent from both Clusters and
// Noise are not present in the returned map.
func (c ClusterResult) ClusterOf() map[string]int {
	m := make(map[string]int, len(c.Noise))
	for id, paths := range c.Clusters {
		for _, p := range paths {
			m[p] = id
		}
	}
	noise := make([]string, len(c.Noise))
	copy(noise, c.Noise)
	sort.Strings(noise)
	for i, p := range noise {
		m[p] = -(i + 1)
	}
	return m
}

// clusterResultFromGroups adapts ScopedCluster output ([][]factForLLM) to the
// ClusterResult the bridge engine consumes. Each group becomes a community
// keyed by its slice index. ScopedCluster has already dropped sub-floor
// communities (filterSmallClusters), so no group is noise here — Noise stays
// empty and facts absent from every group are handled downstream by
// bridgePathCommunities' synthetic-negative-id fallback (identical to the prior
// per-noise-fact assignment).
func clusterResultFromGroups(groups [][]factForLLM) ClusterResult {
	cr := ClusterResult{Clusters: make(map[int][]string, len(groups))}
	for i, g := range groups {
		paths := make([]string, 0, len(g))
		for _, f := range g {
			paths = append(paths, f.File)
		}
		cr.Clusters[i] = paths
	}
	return cr
}
