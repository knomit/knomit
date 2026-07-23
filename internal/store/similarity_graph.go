package store

import (
	"context"
	"fmt"
)

// SimilarityGraph is the member-restricted SIMILAR_TO adjacency for a fixed
// set of fact paths. Built by SimilarityAdjacency; consumed by the bridge
// quality scorer to compute intra-cluster cohesion.
type SimilarityGraph struct {
	adj map[string]map[string]struct{}
}

// Connected reports whether a and b share a SIMILAR_TO edge in the graph.
// The check is symmetric: if a→b was recorded, b→a is also true.
func (g SimilarityGraph) Connected(a, b string) bool {
	n, ok := g.adj[a]
	if !ok {
		return false
	}
	_, ok = n[b]
	return ok
}

// Density returns the fraction of unordered member pairs that are joined by a
// SIMILAR_TO edge. Returns 0 for fewer than 2 members. The denominator is
// n*(n-1)/2 (all distinct unordered pairs among members).
func (g SimilarityGraph) Density(members []string) float64 {
	if len(members) < 2 {
		return 0
	}
	pairs := 0
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if g.Connected(members[i], members[j]) {
				pairs++
			}
		}
	}
	total := len(members) * (len(members) - 1) / 2
	return float64(pairs) / float64(total)
}

// NewSimilarityGraph builds a SimilarityGraph from undirected pairs. Each pair
// [a, b] records a symmetric edge a↔b. Self-pairs (a == b) are ignored.
// Duplicate pairs are idempotent. This constructor is provided so tests and
// offline tools (calibrate) can build non-empty graphs without needing a live
// database; production code uses SimilarityAdjacency instead.
func NewSimilarityGraph(pairs [][2]string) SimilarityGraph {
	g := SimilarityGraph{adj: make(map[string]map[string]struct{})}
	for _, p := range pairs {
		a, b := p[0], p[1]
		if a == b {
			continue
		}
		if g.adj[a] == nil {
			g.adj[a] = make(map[string]struct{})
		}
		g.adj[a][b] = struct{}{}
		if g.adj[b] == nil {
			g.adj[b] = make(map[string]struct{})
		}
		g.adj[b][a] = struct{}{}
	}
	return g
}

// SimilarityAdjacency returns the member-restricted SIMILAR_TO graph for the
// given fact paths. Only edges where BOTH endpoints are in paths are kept.
// Liveness is enforced via NOT n.deleted = true on the neighbor side.
// An empty or single-element paths slice returns an empty graph.
func (gs *graphStore) SimilarityAdjacency(ctx context.Context, paths []string) (SimilarityGraph, error) {
	g := SimilarityGraph{adj: make(map[string]map[string]struct{})}
	if len(paths) < 2 {
		return g, nil
	}

	// Build the member set for O(1) endpoint membership checks.
	memberSet := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		memberSet[p] = struct{}{}
	}

	// For each member fact, find all live SIMILAR_TO neighbours. Both endpoints
	// come back so the member-restriction filter can be applied in Go (keep
	// only edges where both ends are in the input set). Anchors are not
	// deleted-filtered here — only the far endpoint — preserving the previous
	// Cypher predicate. This accessor propagates errors (rather than swallowing
	// them): a downstream cohesion scorer must be able to distinguish "no
	// SIMILAR_TO edges" from "query failed", which would otherwise read as
	// falsely-low cohesion.
	pairs, err := graphSimilarToNeighbours(ctx, conn(ctx, gs.rh.db), paths, false)
	if err != nil {
		return SimilarityGraph{}, fmt.Errorf("SimilarityAdjacency: %w", err)
	}
	for _, pr := range pairs {
		a, b := pr[0], pr[1]
		if a == "" || b == "" {
			continue
		}
		// Both endpoints must be in the input member set.
		if _, ok := memberSet[a]; !ok {
			continue
		}
		if _, ok := memberSet[b]; !ok {
			continue
		}
		// Record symmetric adjacency.
		if g.adj[a] == nil {
			g.adj[a] = make(map[string]struct{})
		}
		g.adj[a][b] = struct{}{}
		if g.adj[b] == nil {
			g.adj[b] = make(map[string]struct{})
		}
		g.adj[b][a] = struct{}{}
	}

	return g, nil
}
