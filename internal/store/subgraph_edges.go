package store

import (
	"context"
	"fmt"
	"strings"
)

// subgraphEdgeChunkSize bounds how many path predicates go into a single Cypher
// MATCH. GraphQLite's SQL translation builds an expression tree from OR-chained
// predicates, and SQLite caps that tree at depth 1000 ("Expression tree is too
// large"). A full-repo review subgraph can be ~hundreds of paths, so the query
// is split into chunks well under the limit. Overridable in tests.
var subgraphEdgeChunkSize = 400

// SubgraphEdges returns the undirected SIMILAR_TO adjacency among the given
// fact paths: one pair per edge whose BOTH endpoints are non-deleted Fact nodes
// in the requested set. Edges leaving the set, and isolated paths, are omitted.
//
// This is the read that scoped clustering runs instead of the old graph-wide
// louvain(): the caller passes the review subgraph (changed facts + their
// nearest neighbours), so the bounded Cypher MATCH returns in milliseconds and
// never holds the write lock the way a full-graph louvain did. Paths are matched
// by name only — callers pass paths already resolved to the branch's visible
// facts, mirroring how ClusterFacts post-filtered by branch_facts path
// membership rather than by node version.
//
// Only the `a` endpoint is restricted (to a chunk of the path set) inside the
// query; the `b` endpoint is filtered against the full set in Go. Every
// intra-set edge (x,y) is therefore found when x's chunk is queried, so
// chunking never drops an edge — it only bounds each query's expression depth.
func (si *searchIndex) SubgraphEdges(ctx context.Context, paths []string) ([][2]string, error) {
	if len(paths) < 2 {
		return nil, nil
	}

	// Distinct path set: membership test for the b endpoint, and the chunk source.
	inSet := make(map[string]bool, len(paths))
	uniq := make([]string, 0, len(paths))
	for _, p := range paths {
		if !inSet[p] {
			inSet[p] = true
			uniq = append(uniq, p)
		}
	}

	out := make([][2]string, 0)
	seen := make(map[string]bool)
	add := func(a, b string) {
		// The undirected `-[:SIMILAR_TO]-` traversal yields each edge in both
		// directions; collapse to one pair per unordered endpoint set.
		x, y := a, b
		if x > y {
			x, y = y, x
		}
		key := x + "\x00" + y
		if !seen[key] {
			seen[key] = true
			out = append(out, [2]string{a, b})
		}
	}

	for start := 0; start < len(uniq); start += subgraphEdgeChunkSize {
		end := start + subgraphEdgeChunkSize
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]

		// OR-chained path equality for the `a` endpoint over this chunk.
		// Parameterised IN-lists are not supported by the installed GraphQLite
		// build (matching graphExpandSearch); each path is escaped with
		// escapeCypherKey — the same helper every other cypher('...') query in this
		// package uses. It escapes the Cypher "..." layer (\, ") AND strips the
		// single quote that would otherwise terminate the outer SQL cypher('...')
		// string literal, so a path with a quote can't break out of either layer.
		// NOT x.deleted = true (rather than = false): GraphQLite stores booleans as
		// JSON booleans that never compare equal to a Cypher literal false, so the
		// negated form is how graph reads exclude soft-deleted nodes.
		parts := make([]string, 0, len(chunk))
		for _, p := range chunk {
			parts = append(parts, fmt.Sprintf(`a.path = "%s"`, escapeCypherKey(p)))
		}
		q := fmt.Sprintf(
			`SELECT json_extract(value, '$.a') AS a, json_extract(value, '$.b') AS b
			 FROM json_each(cypher('MATCH (a:%s)-[:%s]-(b:%s) WHERE (%s) AND NOT a.deleted = true AND NOT b.deleted = true RETURN DISTINCT a.path AS a, b.path AS b'))`,
			NodeFact, EdgeSimilarTo, NodeFact, strings.Join(parts, " OR "),
		)

		// Best-effort cypher read; retry the transient concurrent-translation
		// race. Dedup via `seen` makes a retry that re-scans rows idempotent.
		err := withCypherRetry(func() error {
			rows, qerr := conn(ctx, si.rh.db).QueryContext(ctx, q)
			if qerr != nil {
				return qerr
			}
			defer rows.Close()
			for rows.Next() {
				var a, b string
				if serr := rows.Scan(&a, &b); serr != nil {
					continue
				}
				// Keep only edges whose far endpoint is also in the subgraph.
				if a == "" || b == "" || a == b || !inSet[b] {
					continue
				}
				add(a, b)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, fmt.Errorf("subgraph edges: %w", err)
		}
	}
	return out, nil
}
