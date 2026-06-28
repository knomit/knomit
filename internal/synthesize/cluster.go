// Package synthesize — Scoped clustering: builds clusters containing only seed
// facts and their nearest neighbors, then runs Louvain community detection on
// the subgraph. Used by the review orchestrator to focus on changed facts.
package synthesize

import (
	"context"
	"path"
	"sync"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
	"knomit/internal/store"
)

// maxConcurrentNeighborSearches caps the number of in-flight idx.Search
// calls during scoped clustering. Each search is I/O-bound (vector query +
// optional embedding) so a small worker pool keeps wall-clock time low
// without overwhelming the search backend.
const maxConcurrentNeighborSearches = 8

// defaultScopedResolution / defaultScopedMinCommunitySize are defensive
// fallbacks used only when a caller passes a non-positive value. The real
// values come from config ([cluster_cache] resolution/min_community_size) via
// RepoInstance and must stay in sync with repos.defaultCluster* — kept as plain
// constants here because synthesize must not import repos.
const (
	defaultScopedResolution       = 4.0
	defaultScopedMinCommunitySize = 2
)

// ScopedCluster builds clusters containing only seed facts and their nearest neighbors.
// Algorithm:
// 1. For each seed, find neighbors via idx.Search (semantic similarity) scoped to same category
// 2. Build subgraph of seeds + neighbors
// 3. Run Louvain (gonum) over the subgraph's SIMILAR_TO edges (idx.SubgraphEdges) in-process
// 4. Fallback to grouping by category path if the edge read fails or yields no clusters
func ScopedCluster(ctx context.Context,
	seeds []factForLLM,
	idx store.SearchIndex,
	resolution float64,
	minCommunitySize int,
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

	// Step 1: Build subgraph of seeds + neighbors. Searches are dispatched
	// concurrently with bounded parallelism since each idx.Search is
	// independent and dominates wall-clock time. A single mutex protects the
	// shared subgraph and factByPath maps; goroutines that fail their search
	// just skip (matching the serial behavior).
	subgraph := make(map[string]bool)
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentNeighborSearches)
	for _, seed := range seeds {
		g.Go(func() error {
			cat := categoryDir(seed.File)
			// QueryByPath skips ONNX embedding entirely — the seed already
			// has a stored vector in facts_vec from when it was learned.
			// One SQL statement (subquery resolves the source vector inside
			// the MATCH operand) replaces an embedding inference + KNN.
			results, err := idx.Search(gctx, agentBranch, store.SearchOptions{
				QueryByPath:  seed.File,
				Path:         cat,
				Limit:        10,
				ExcludeTypes: excludeTypes,
			})

			mu.Lock()
			defer mu.Unlock()
			subgraph[seed.File] = true
			if err != nil {
				log.Debug().Err(err).Str("seed", seed.File).Msg("scoped-cluster: neighbor search failed")
				return nil
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
			return nil
		})
	}
	// Goroutines never return errors (we swallow search failures), so Wait
	// here is just a barrier; checking err for completeness.
	if err := g.Wait(); err != nil {
		return nil, err
	}

	onProgress(ProgressEvent{Phase: "cluster", Message: "scoped clustering: subgraph built"})

	// Step 2: Run Louvain over JUST this subgraph, in-process. The clustering
	// the reviewer needs is "group these changed facts + neighbours by mutual
	// similarity" — a small-subgraph question. Fetching the SIMILAR_TO edges
	// among the subgraph (a bounded, milliseconds Cypher read) and running
	// gonum Louvain on them replaces the old graph-wide louvain() that clustered
	// the entire repo graph and discarded all but these paths.
	// Defensive fallbacks; callers pass the config-driven values via RepoInstance
	// (config [cluster_cache] resolution/min_community_size) — the source of truth.
	if resolution <= 0 {
		resolution = defaultScopedResolution
	}
	if minCommunitySize <= 0 {
		minCommunitySize = defaultScopedMinCommunitySize
	}

	paths := make([]string, 0, len(subgraph))
	for p := range subgraph {
		paths = append(paths, p)
	}

	edges, err := idx.SubgraphEdges(ctx, paths)
	if err != nil {
		log.Debug().Err(err).Msg("scoped-cluster: subgraph edge read failed, falling back to category grouping")
		onProgress(ProgressEvent{Phase: "cluster", Message: "edge read failed, using category fallback"})
		return filterSmallClusters(groupByCategory(subgraphFacts(subgraph, factByPath)), minCommunitySize), nil
	}

	// Map each Louvain community of paths back to the facts the reviewer holds.
	var clusters [][]factForLLM
	for _, group := range louvainCommunities(paths, edges, resolution) {
		var fg []factForLLM
		for _, p := range group {
			if f, ok := factByPath[p]; ok {
				fg = append(fg, f)
			}
		}
		if len(fg) > 0 {
			clusters = append(clusters, fg)
		}
	}

	if len(clusters) == 0 {
		log.Debug().Msg("scoped-cluster: no clusters in subgraph, falling back to category grouping")
		onProgress(ProgressEvent{Phase: "cluster", Message: "no clusters in subgraph, using category fallback"})
		return filterSmallClusters(groupByCategory(subgraphFacts(subgraph, factByPath)), minCommunitySize), nil
	}

	onProgress(ProgressEvent{Phase: "cluster", Message: "scoped clustering complete"})
	return filterSmallClusters(clusters, minCommunitySize), nil
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

// filterSmallClusters removes clusters smaller than minCommunitySize, honouring
// the configured cluster minimum so the category-fallback paths drop the same
// small communities the Louvain path does (rather than a hardcoded "< 2").
func filterSmallClusters(clusters [][]factForLLM, minCommunitySize int) [][]factForLLM {
	var out [][]factForLLM
	for _, c := range clusters {
		if len(c) >= minCommunitySize {
			out = append(out, c)
		}
	}
	return out
}
