// Package synthesize — Scoped clustering: builds clusters containing only seed
// facts and their nearest neighbors, then runs Louvain community detection on
// the subgraph. Used by the review orchestrator to focus on changed facts.
package synthesize

import (
	"path"

	"github.com/rs/zerolog/log"
	"knomit/internal/store"
)

// ScopedCluster builds clusters containing only seed facts and their nearest neighbors.
// Algorithm:
// 1. For each seed, find neighbors via idx.Search (semantic similarity) scoped to same category
// 2. Build subgraph of seeds + neighbors
// 3. Run Louvain clustering (idx.ClusterFacts) over the full graph, then filter to subgraph paths
// 4. Fallback to grouping by category path if Louvain fails or no embeddings
func ScopedCluster(
	seeds []factForLLM,
	idx SearchIndex,
	resolution float64,
	onProgress func(ProgressEvent),
	agentBranch string,
	excludeTypes ...string,
) ([][]factForLLM, error) {
	if len(seeds) == 0 {
		return nil, nil
	}

	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}

	// Build lookup from path to fact, starting with seeds.
	factByPath := make(map[string]factForLLM, len(seeds)*2)
	for _, s := range seeds {
		factByPath[s.File] = s
	}

	// Step 1: Build subgraph of seeds + neighbors.
	subgraph := make(map[string]bool)
	for _, seed := range seeds {
		subgraph[seed.File] = true

		cat := categoryDir(seed.File)
		results, err := idx.Search(agentBranch, store.SearchQuery{
			Text:         seed.Title + " " + seed.Body,
			Path:         cat,
			Limit:        10,
			ExcludeTypes: excludeTypes,
		})
		if err != nil {
			log.Debug().Err(err).Str("seed", seed.File).Msg("scoped-cluster: neighbor search failed")
			continue
		}
		for _, r := range results {
			subgraph[r.Path] = true
			if _, exists := factByPath[r.Path]; !exists {
				factByPath[r.Path] = factForLLM{
					File: r.Path, Title: r.Title, Body: r.Body,
					Type: r.Type, Domain: r.Domain, Entities: r.Entities,
					Confidence: r.Confidence, Sources: r.Sources,
				}
			}
		}
	}

	onProgress(ProgressEvent{Phase: "cluster", Message: "scoped clustering: subgraph built"})

	// Step 2: Try Louvain clustering on the full graph, then filter to subgraph.
	if resolution <= 0 {
		resolution = 1.0
	}

	result, err := idx.ClusterFacts(resolution, 2)
	if err != nil {
		log.Debug().Err(err).Msg("scoped-cluster: Louvain failed, falling back to category grouping")
		onProgress(ProgressEvent{Phase: "cluster", Message: "Louvain failed, using category fallback"})
		return filterSmallClusters(groupByCategory(subgraphFacts(subgraph, factByPath))), nil
	}

	// Filter Louvain clusters to only include subgraph paths.
	var clusters [][]factForLLM
	for _, paths := range result.Clusters {
		var group []factForLLM
		for _, p := range paths {
			if subgraph[p] {
				if f, ok := factByPath[p]; ok {
					group = append(group, f)
				}
			}
		}
		if len(group) > 0 {
			clusters = append(clusters, group)
		}
	}

	if len(clusters) == 0 {
		log.Debug().Msg("scoped-cluster: no Louvain clusters in subgraph, falling back to category grouping")
		onProgress(ProgressEvent{Phase: "cluster", Message: "no clusters in subgraph, using category fallback"})
		return filterSmallClusters(groupByCategory(subgraphFacts(subgraph, factByPath))), nil
	}

	onProgress(ProgressEvent{Phase: "cluster", Message: "scoped clustering complete"})
	return filterSmallClusters(clusters), nil
}

// categoryDir extracts the parent directory from a fact path.
// e.g. "kb/go/concurrency/uuid.md" -> "kb/go/concurrency"
func categoryDir(p string) string {
	return path.Dir(p)
}

// groupByCategory groups facts by their parent directory.
func groupByCategory(facts []factForLLM) [][]factForLLM {
	groups := make(map[string][]factForLLM)
	for _, f := range facts {
		cat := categoryDir(f.File)
		groups[cat] = append(groups[cat], f)
	}
	result := make([][]factForLLM, 0, len(groups))
	for _, g := range groups {
		result = append(result, g)
	}
	return result
}

// subgraphFacts returns factForLLM values for all paths in the subgraph set.
func subgraphFacts(subgraph map[string]bool, factByPath map[string]factForLLM) []factForLLM {
	facts := make([]factForLLM, 0, len(subgraph))
	for p := range subgraph {
		if f, ok := factByPath[p]; ok {
			facts = append(facts, f)
		}
	}
	return facts
}

// filterSmallClusters removes clusters with fewer than 2 facts.
func filterSmallClusters(clusters [][]factForLLM) [][]factForLLM {
	var out [][]factForLLM
	for _, c := range clusters {
		if len(c) > 1 {
			out = append(out, c)
		}
	}
	return out
}
