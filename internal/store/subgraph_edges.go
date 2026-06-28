package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SubgraphEdges returns the undirected SIMILAR_TO adjacency among the given
// fact paths: one pair per edge whose BOTH endpoints are non-deleted Fact nodes
// in the requested set. Edges leaving the set, and isolated paths, are omitted.
//
// This is the read that scoped clustering runs instead of the old graph-wide
// louvain(): the caller passes the review subgraph (changed facts + their
// nearest neighbours, tens of nodes), so the bounded Cypher MATCH returns in
// milliseconds and never holds the write lock the way a full-graph louvain did.
// Paths are matched by name only — callers pass paths already resolved to the
// branch's visible facts, mirroring how ClusterFacts post-filtered by
// branch_facts path membership rather than by node version.
func (si *searchIndex) SubgraphEdges(ctx context.Context, paths []string) ([][2]string, error) {
	if len(paths) < 2 {
		return nil, nil
	}

	// OR-chained path equality for one endpoint variable. Parameterised IN-lists
	// are not supported by the installed GraphQLite build, matching
	// graphExpandSearch's approach. Each path is JSON-encoded for safe quoting.
	endpointFilter := func(varName string) string {
		seen := make(map[string]bool, len(paths))
		parts := make([]string, 0, len(paths))
		for _, p := range paths {
			if seen[p] {
				continue
			}
			seen[p] = true
			b, _ := json.Marshal(p)
			parts = append(parts, varName+".path = "+string(b))
		}
		return strings.Join(parts, " OR ")
	}

	// NOT x.deleted = true (rather than = false): GraphQLite stores booleans as
	// JSON booleans that never compare equal to a Cypher literal false, so the
	// negated form is how the existing graph reads exclude soft-deleted nodes.
	q := fmt.Sprintf(
		`SELECT json_extract(value, '$.a') AS a, json_extract(value, '$.b') AS b
		 FROM json_each(cypher('MATCH (a:%s)-[:%s]-(b:%s) WHERE (%s) AND (%s) AND NOT a.deleted = true AND NOT b.deleted = true RETURN DISTINCT a.path AS a, b.path AS b'))`,
		NodeFact, EdgeSimilarTo, NodeFact, endpointFilter("a"), endpointFilter("b"),
	)

	var out [][2]string
	seen := make(map[string]bool)
	// Best-effort cypher read; retry the transient concurrent-translation race.
	// The closure rebuilds out/seen each attempt so a retry after a partial scan
	// is idempotent.
	err := withCypherRetry(func() error {
		out = nil
		clear(seen)
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
			if a == "" || b == "" || a == b {
				continue
			}
			// The undirected `-[:SIMILAR_TO]-` traversal yields each edge in both
			// directions; collapse to one pair per unordered endpoint set.
			x, y := a, b
			if x > y {
				x, y = y, x
			}
			key := x + "\x00" + y
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, [2]string{a, b})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("subgraph edges: %w", err)
	}
	return out, nil
}
