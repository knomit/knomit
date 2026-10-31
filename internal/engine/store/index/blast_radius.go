package index

import (
	"context"
	"fmt"
	"strings"
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
//
// The walk is over BLOBS: reference edges belong to a blob and sit on the
// node that first carried it, so each step widens to every node of the same
// blob (a rename's node at another path) before following edges, and the
// dependents' paths are read from every node of their blobs.
//
// The walk reads ref_edges (target_blob -> source_blob through
// idx_ref_edges_target, local rows only) once the table is backfilled, and
// the DERIVED_FROM edges before that (PR #694 review N2), or when useTable is
// false (a test pins the two equal). Both give the same set: the rows mirror
// the edges, and a row's target_blob is the blob of the edge's target node.
func (gs *GraphStore) reverseDependentPaths(ctx context.Context, path string, useTable bool) (map[string]struct{}, error) {
	db := conn(ctx, gs.rh.DB())
	q, args := reverseDependentsByRowsSQL, []any{NodeFact, path, path}
	if !useTable || !refEdgesReady(ctx, db) {
		q, args = reverseDependentsByEdgesSQL, []any{NodeFact, path, EdgeDerivedFrom, EdgeDerivedFrom, path}
	}
	rows, err := db.QueryContext(ctx, q, args...)
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

// reverseDependentsByRowsSQL is the walk over ref_edges.
const reverseDependentsByRowsSQL = `
WITH RECURSIVE
  kb(id) AS (SELECT id FROM property_keys WHERE key = 'blob_hash'),
  seed(blob) AS (
    SELECT nb.value
      FROM node_props_text np
      JOIN property_keys k ON k.id = np.key_id AND k.key = 'path'
      JOIN node_labels nl ON nl.node_id = np.node_id AND nl.label = ?
      JOIN node_props_text nb ON nb.node_id = np.node_id AND nb.key_id = (SELECT id FROM kb)
     WHERE np.value = ?
  ),
  deps(blob) AS (
    SELECT r.source_blob
      FROM seed s
      JOIN ref_edges r ON r.target_blob = s.blob AND r.target_repo = ''
    UNION
    SELECT r.source_blob
      FROM deps d
      JOIN ref_edges r ON r.target_blob = d.blob AND r.target_repo = ''
  )
SELECT DISTINCT np.value
  FROM deps
  JOIN node_props_text nb ON nb.key_id = (SELECT id FROM kb) AND nb.value = deps.blob
  JOIN node_props_text np ON np.node_id = nb.node_id
  JOIN property_keys k ON k.id = np.key_id AND k.key = 'path'
 WHERE np.value <> ?`

// reverseDependentsByEdgesSQL is the walk over the DERIVED_FROM edges.
const reverseDependentsByEdgesSQL = `
WITH RECURSIVE
  kb(id) AS (SELECT id FROM property_keys WHERE key = 'blob_hash'),
  seed(blob) AS (
    SELECT nb.value
      FROM node_props_text np
      JOIN property_keys k ON k.id = np.key_id AND k.key = 'path'
      JOIN node_labels nl ON nl.node_id = np.node_id AND nl.label = ?
      JOIN node_props_text nb ON nb.node_id = np.node_id AND nb.key_id = (SELECT id FROM kb)
     WHERE np.value = ?
  ),
  deps(blob) AS (
    SELECT sb.value
      FROM seed s
      JOIN node_props_text tb ON tb.key_id = (SELECT id FROM kb) AND tb.value = s.blob
      JOIN edges e ON e.target_id = tb.node_id AND e.type = ?
      JOIN node_props_text sb ON sb.node_id = e.source_id AND sb.key_id = (SELECT id FROM kb)
    UNION
    SELECT sb.value
      FROM deps d
      JOIN node_props_text tb ON tb.key_id = (SELECT id FROM kb) AND tb.value = d.blob
      JOIN edges e ON e.target_id = tb.node_id AND e.type = ?
      JOIN node_props_text sb ON sb.node_id = e.source_id AND sb.key_id = (SELECT id FROM kb)
  )
SELECT DISTINCT np.value
  FROM deps
  JOIN node_props_text nb ON nb.key_id = (SELECT id FROM kb) AND nb.value = deps.blob
  JOIN node_props_text np ON np.node_id = nb.node_id
  JOIN property_keys k ON k.id = np.key_id AND k.key = 'path'
 WHERE np.value <> ?`

// ReverseDependentPaths returns all paths transitively DERIVED_FROM any version
// of `path` (all historical versions are seeded), NOT liveness-filtered.
// Membership among live members is the consumer's responsibility.
// It is a thin exported wrapper over the private reverseDependentPaths.
func (gs *GraphStore) ReverseDependentPaths(ctx context.Context, path string) (map[string]struct{}, error) {
	if err := gs.probe("ReverseDependentPaths"); err != nil {
		return nil, err
	}
	return gs.reverseDependentPaths(ctx, path, true)
}

// BlastRadius counts the distinct facts that are LIVE on `branch` at HEAD and
// transitively derive from any version of `path`. It is the keystone-impact
// metric: how much of the live corpus would be invalidated if `path` were
// false. Returns 0 for a leaf fact (nothing depends on it), or for a path
// whose dependents are all retracted at HEAD.
func (gs *GraphStore) BlastRadius(ctx context.Context, branch, path string) (int, error) {
	gs.probe("BlastRadius")
	return gs.blastRadius(ctx, branch, path, true)
}

// blastRadius is BlastRadius over ref_edges (useTable, once backfilled) or the
// DERIVED_FROM edges.
func (gs *GraphStore) blastRadius(ctx context.Context, branch, path string, useTable bool) (int, error) {
	deps, err := gs.reverseDependentPaths(ctx, path, useTable)
	if err != nil {
		return 0, err
	}
	if len(deps) == 0 {
		return 0, nil
	}
	branchID, err := gs.rh.BranchID(ctx, branch)
	if err != nil {
		return 0, fmt.Errorf("BlastRadius: branchID: %w", err)
	}
	paths := make([]string, 0, len(deps))
	for p := range deps {
		paths = append(paths, p)
	}
	// Batch into chunks of 999 (SQLite SQLITE_MAX_VARIABLE_NUMBER limit).
	const maxBatch = 999
	count := 0
	for i := 0; i < len(paths); i += maxBatch {
		end := i + maxBatch
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[i:end]
		ph := strings.Repeat("?,", len(batch))
		ph = ph[:len(ph)-1] // strip trailing comma
		args := make([]interface{}, 0, 1+len(batch))
		args = append(args, branchID)
		for _, p := range batch {
			args = append(args, p)
		}
		var n int
		q := "SELECT COUNT(*) FROM branch_facts WHERE branch_id = ? AND path IN (" + ph + ")"
		if err := conn(ctx, gs.rh.DB()).QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("BlastRadius: liveness count: %w", err)
		}
		count += n
	}
	return count, nil
}
