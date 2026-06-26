package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// reverseDependentPaths returns the set of distinct fact paths that
// transitively derive (DERIVED_FROM, any depth) from ANY historical version
// of `path`, excluding `path` itself.
//
// DERIVED_FROM points source → target (source derives FROM target); the
// reverse-reachability walk follows edges where target_id is in the frontier
// and collects source_id. UNION (not UNION ALL) dedupes node ids and
// terminates on cycles. Path resolution comes from node_props_text; liveness
// filtering is the caller's job (see BlastRadius).
//
// The seed set is every Fact node carrying `path` as its `path` property —
// i.e. every version of the path, not just HEAD. The historical-graph
// invariant means a retracted intermediate's node + edges persist; a
// dependent declared against an older version of the target still transmits
// reach.
func (si *searchIndex) reverseDependentPaths(ctx context.Context, path string) (map[string]struct{}, error) {
	const q = `
WITH RECURSIVE
  seed(node_id) AS (
    SELECT np.node_id
      FROM node_props_text np
      JOIN property_keys k ON k.id = np.key_id AND k.key = 'path'
      JOIN node_labels nl ON nl.node_id = np.node_id AND nl.label = ?
     WHERE np.value = ?
  ),
  deps(node_id) AS (
    SELECT e.source_id
      FROM edges e
      JOIN seed s ON s.node_id = e.target_id
     WHERE e.type = ?
    UNION
    SELECT e.source_id
      FROM edges e
      JOIN deps d ON d.node_id = e.target_id
     WHERE e.type = ?
  )
SELECT DISTINCT np.value
  FROM deps
  JOIN node_props_text np ON np.node_id = deps.node_id
  JOIN property_keys k ON k.id = np.key_id AND k.key = 'path'
 WHERE np.value <> ?`
	rows, err := conn(ctx, si.rh.db).QueryContext(ctx, q,
		NodeFact, path, EdgeDerivedFrom, EdgeDerivedFrom, path)
	if err != nil {
		return nil, fmt.Errorf("reverseDependentPaths %q: %w", path, err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("reverseDependentPaths %q: scan: %w", path, err)
		}
		out[p] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reverseDependentPaths %q: rows: %w", path, err)
	}
	return out, nil
}

// BlastRadius counts the distinct facts that are LIVE on `branch` at HEAD and
// transitively derive from any version of `path`. It is the keystone-impact
// metric: how much of the live corpus would be invalidated if `path` were
// false. Returns 0 for a leaf fact (nothing depends on it), or for a path
// whose dependents are all retracted at HEAD.
func (si *searchIndex) BlastRadius(ctx context.Context, branch, path string) (int, error) {
	deps, err := si.reverseDependentPaths(ctx, path)
	if err != nil {
		return 0, err
	}
	if len(deps) == 0 {
		return 0, nil
	}
	branchID, err := si.rh.branchID(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("BlastRadius: branchID: %w", err)
	}
	count := 0
	for p := range deps {
		var one int
		err := conn(ctx, si.rh.db).QueryRowContext(ctx,
			`SELECT 1 FROM branch_facts WHERE branch_id = ? AND path = ?`,
			branchID, p,
		).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("BlastRadius: liveness %q: %w", p, err)
		}
		count++
	}
	return count, nil
}
