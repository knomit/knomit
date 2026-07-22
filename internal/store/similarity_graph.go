package store

import (
	"context"
	"fmt"
	"strings"
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

	// Build OR-chained path filter. Each path is escaped with escapeCypherKey —
	// the same helper every other cypher('...') query in this package uses. It
	// escapes the Cypher "..." layer (\, ") AND strips the single quote that
	// would otherwise terminate the outer SQL cypher('...') string literal, so a
	// path with a quote can't break out of either layer (SQL/Cypher injection).
	// Parameterized queries are not used because the installed GraphQLite build
	// does not support variadic OR patterns.
	pathParts := make([]string, 0, len(paths))
	for _, p := range paths {
		pathParts = append(pathParts, fmt.Sprintf(`f.path = "%s"`, escapeCypherKey(p)))
	}
	pathFilter := strings.Join(pathParts, " OR ")

	// Query: for each member fact f, find all live SIMILAR_TO neighbors n.
	// We return both endpoints so we can apply the member-restriction filter
	// in Go (keep only edges where both ends are in the input set).
	// NOT n.deleted = true is used instead of n.deleted = false because
	// GraphQLite stores booleans as JSON booleans which do not compare equal
	// to Cypher literal false; non-deleted nodes have deleted=false (set in
	// graphSyncFact), so this correctly excludes soft-deleted nodes.
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.a'), json_extract(value, '$.b') FROM json_each(cypher('MATCH (f:%s)-[:%s]-(n:%s) WHERE (%s) AND NOT n.deleted = true RETURN DISTINCT f.path AS a, n.path AS b'))`,
		NodeFact, EdgeSimilarTo, NodeFact, pathFilter,
	)

	// Cypher read with retry for the transient concurrent-translation race.
	// Map updates are idempotent so a retry after a partial first attempt is
	// safe. This accessor propagates the error (rather than swallowing it): a
	// downstream cohesion scorer must be able to distinguish "no SIMILAR_TO
	// edges" from "query failed" (which would otherwise read as falsely-low
	// cohesion).
	if err := withCypherRetry(func() error {
		// Clear any partial results from a previous attempt.
		for k := range g.adj {
			delete(g.adj, k)
		}

		rows, err := conn(ctx, gs.rh.db).QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				// A Scan failure on a non-nil row is a schema mismatch, not a
				// transient race — surface it rather than silently producing a
				// partial graph.
				return fmt.Errorf("scan SIMILAR_TO row: %w", err)
			}
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
		return rows.Err()
	}); err != nil {
		return SimilarityGraph{}, fmt.Errorf("SimilarityAdjacency: %w", err)
	}

	return g, nil
}
