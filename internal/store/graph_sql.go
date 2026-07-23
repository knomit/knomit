package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	storegit "knomit/internal/store/git"
)

// Direct-SQL graph write primitives over the EAV tables (nodes, node_labels,
// edges, property_keys, node_props_text, edge_props_text). These replace the
// string-interpolated cypher() write path.
//
// MERGE SEMANTICS ARE LOAD-BEARING. Rebuild re-runs the same writes on every
// pass, so graphMergeNode/graphMergeEdge must be idempotent by identity; a
// blind INSERT would duplicate the whole graph on each rebuild.
//
// What is permanent and what is rewritten:
//   - Nodes are merge-only. Fact nodes are per-version and retained forever
//     (the graph is temporal); retraction sets deleted='true' rather than
//     removing them. graphDetachDeleteNode exists for GC of orphans only.
//   - DERIVED_FROM edges are immutable historical assertions of lineage at a
//     commit. They are never pruned — they cannot be recomputed once dropped.
//   - The relationship edges of a fact version (TAGGED, IN_DOMAIN, UNDER) are
//     delete-then-remerge on each write, scoped to that version's node.
//   - SIMILAR_TO is pure derived data, recomputed in full from facts_vec: the
//     incremental path rewrites one source's outgoing edges, and Rebuild wipes
//     the type globally before re-merging (see graphDeleteEdgesByType).
//
// All properties are stored as TEXT in {node,edge}_props_text. The typed prop
// tables the GraphQLite extension used (_int/_real/_bool/_json) are not written:
// every graph read is projection or equality only, and the sole non-text
// predicate (deleted) is satisfied by comparing against 'true'/'false'.

// graphPropKeyIDs ensures a property_keys row exists for every key and returns
// the key→id mapping.
//
// Resolved in bulk — one multi-row INSERT OR IGNORE plus one IN-list SELECT —
// rather than a pair of statements per key. A fact write sets six properties at
// once, so the per-key form cost twelve round-trips where two suffice. Ids are
// deliberately NOT cached in the process: they are per-database, and knomit
// opens one Service per repo, so a shared cache would hand one database's ids
// to another.
func graphPropKeyIDs(ctx context.Context, ex storegit.CtxExecer, keys []string) (map[string]int64, error) {
	if len(keys) == 0 {
		return map[string]int64{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("(?),", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, k)
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT OR IGNORE INTO property_keys(key) VALUES `+placeholders, args...); err != nil {
		return nil, fmt.Errorf("graphPropKeyIDs: ensure keys: %w", err)
	}

	rows, err := ex.QueryContext(ctx,
		`SELECT key, id FROM property_keys WHERE key IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("graphPropKeyIDs: lookup: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64, len(keys))
	for rows.Next() {
		var key string
		var id int64
		if err := rows.Scan(&key, &id); err != nil {
			return nil, fmt.Errorf("graphPropKeyIDs: scan: %w", err)
		}
		out[key] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphPropKeyIDs: %w", err)
	}
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("graphPropKeyIDs: key %q missing after insert", k)
		}
	}
	return out, nil
}

// sortedKeys returns map keys in deterministic order so generated SQL and
// parameter order are stable.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// graphNodeIDByProps returns the id of the node carrying `label` and ALL the
// given identity properties, or 0 when no such node exists. This is the
// generalised form of graphNodeIDByBlob.
func graphNodeIDByProps(ctx context.Context, ex storegit.CtxExecer, label string, identity map[string]string) (int64, error) {
	if len(identity) == 0 {
		return 0, fmt.Errorf("graphNodeIDByProps: empty identity for label %q", label)
	}
	keys := sortedKeys(identity)

	var sb strings.Builder
	sb.WriteString(`SELECT nl.node_id FROM node_labels nl`)
	args := []any{}
	for i, k := range keys {
		fmt.Fprintf(&sb,
			" JOIN node_props_text p%d ON p%d.node_id = nl.node_id"+
				" JOIN property_keys k%d ON k%d.id = p%d.key_id AND k%d.key = ?",
			i, i, i, i, i, i)
		args = append(args, k)
	}
	sb.WriteString(` WHERE nl.label = ?`)
	args = append(args, label)
	for i, k := range keys {
		fmt.Fprintf(&sb, " AND p%d.value = ?", i)
		args = append(args, identity[k])
	}
	sb.WriteString(` LIMIT 1`)

	var nodeID int64
	err := ex.QueryRowContext(ctx, sb.String(), args...).Scan(&nodeID)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("graphNodeIDByProps(%s): %w", label, err)
	}
	return nodeID, nil
}

// graphMergeNode finds-or-creates the node with `label` and the given identity
// properties, returning its id. Equivalent to cypher MERGE (n:Label {k: v, …}).
// Idempotent: repeated calls with the same identity return the same id.
func graphMergeNode(ctx context.Context, ex storegit.CtxExecer, label string, identity map[string]string) (int64, error) {
	existing, err := graphNodeIDByProps(ctx, ex, label, identity)
	if err != nil {
		return 0, err
	}
	if existing != 0 {
		return existing, nil
	}

	res, err := ex.ExecContext(ctx, `INSERT INTO nodes DEFAULT VALUES`)
	if err != nil {
		return 0, fmt.Errorf("graphMergeNode(%s): insert node: %w", label, err)
	}
	nodeID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("graphMergeNode(%s): last id: %w", label, err)
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT OR IGNORE INTO node_labels(node_id, label) VALUES (?, ?)`, nodeID, label); err != nil {
		return 0, fmt.Errorf("graphMergeNode(%s): label: %w", label, err)
	}
	if err := graphSetNodeProps(ctx, ex, nodeID, identity); err != nil {
		return 0, fmt.Errorf("graphMergeNode(%s): identity props: %w", label, err)
	}
	return nodeID, nil
}

// graphSetNodeProps writes text properties on a node, replacing any existing
// value for the same key. Equivalent to cypher SET n.k = v.
func graphSetNodeProps(ctx context.Context, ex storegit.CtxExecer, nodeID int64, props map[string]string) error {
	keys := sortedKeys(props)
	keyIDs, err := graphPropKeyIDs(ctx, ex, keys)
	if err != nil {
		return fmt.Errorf("graphSetNodeProps: %w", err)
	}
	for _, key := range keys {
		if _, err := ex.ExecContext(ctx,
			`INSERT OR REPLACE INTO node_props_text(node_id, key_id, value) VALUES (?, ?, ?)`,
			nodeID, keyIDs[key], props[key]); err != nil {
			return fmt.Errorf("graphSetNodeProps: set %q: %w", key, err)
		}
	}
	return nil
}

// graphMergeEdge creates an edge of edgeType from srcID to tgtID unless an
// identical one already exists. Equivalent to cypher MERGE (a)-[:TYPE]->(b).
//
// NOTE: this is for the single-edge-per-(src,tgt,type) relationships
// (TAGGED, IN_DOMAIN, UNDER, SIMILAR_TO, *_CHILD_OF). DERIVED_FROM is
// deliberately a multi-edge — it carries per-commit properties — and keeps its
// own insert + property-aware dedup guard in derived_from.go.
//
// The existence check and the insert are ONE statement, so they evaluate under
// a single implicit transaction. Split across two statements this is not atomic
// in autocommit: graphBuildSimilarityEdges calls this on the bare pool, and
// `edges` has no uniqueness constraint to catch two writers that both observed
// "absent" before either inserted.
func graphMergeEdge(ctx context.Context, ex storegit.CtxExecer, srcID, tgtID int64, edgeType string) error {
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO edges(source_id, target_id, type)
		SELECT ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM edges WHERE source_id = ? AND target_id = ? AND type = ?
		)`, srcID, tgtID, edgeType, srcID, tgtID, edgeType); err != nil {
		return fmt.Errorf("graphMergeEdge(%s): insert: %w", edgeType, err)
	}
	return nil
}

// graphDeleteOutgoingEdges removes all edgeType edges leaving nodeID.
// Equivalent to cypher MATCH (n)-[r:TYPE]->() DELETE r.
func graphDeleteOutgoingEdges(ctx context.Context, ex storegit.CtxExecer, nodeID int64, edgeType string) error {
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM edges WHERE source_id = ? AND type = ?`, nodeID, edgeType); err != nil {
		return fmt.Errorf("graphDeleteOutgoingEdges(%s): %w", edgeType, err)
	}
	return nil
}

// graphDeleteEdgesByType removes every edge of edgeType across the whole graph.
//
// Only for edge types that are PURE DERIVED DATA, recomputed in full by the
// caller in the same transaction — today that is SIMILAR_TO during Rebuild.
// Never call this for DERIVED_FROM: those edges are immutable historical
// assertions of lineage at a commit and cannot be recomputed once dropped.
//
// The delete is deliberately global rather than branch-scoped, because the
// recomputation that follows it is global too (Rebuild's fact set is the
// COW-global `facts` table, not a branch projection).
func graphDeleteEdgesByType(ctx context.Context, ex storegit.CtxExecer, edgeType string) error {
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM edges WHERE type = ?`, edgeType); err != nil {
		return fmt.Errorf("graphDeleteEdgesByType(%s): %w", edgeType, err)
	}
	return nil
}

// graphDeleteIncomingEdges removes all edgeType edges entering nodeID.
// Equivalent to cypher MATCH ()-[r:TYPE]->(n) DELETE r.
func graphDeleteIncomingEdges(ctx context.Context, ex storegit.CtxExecer, nodeID int64, edgeType string) error {
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM edges WHERE target_id = ? AND type = ?`, nodeID, edgeType); err != nil {
		return fmt.Errorf("graphDeleteIncomingEdges(%s): %w", edgeType, err)
	}
	return nil
}

// graphDerivedFromNeighbours returns the DERIVED_FROM edges incident to the
// Fact version (path, blobHash), described from the far endpoint's side.
//
// incoming=true  → edges INTO that version; each RefSummary describes the
// SOURCE fact and carries the edge's source_commit.
// incoming=false → edges OUT of it; describes the TARGET fact and carries
// target_commit.
//
// Replaces the two json_each(cypher(...)) readers. Title/type/deleted and the
// commit property are optional, so they are LEFT JOINed and default to "".
func graphDerivedFromNeighbours(ctx context.Context, ex storegit.CtxExecer, path, blobHash string, incoming bool) ([]RefSummary, error) {
	anchorCol, otherCol, commitKey := "target_id", "source_id", "source_commit"
	if !incoming {
		anchorCol, otherCol, commitKey = "source_id", "target_id", "target_commit"
	}

	// anchorCol/otherCol are internal constants, never caller input.
	q := fmt.Sprintf(`
		SELECT op.value,
		       COALESCE(ot.value, ''),
		       COALESCE(oty.value, ''),
		       COALESCE(ec.value, ''),
		       COALESCE(od.value, '')
		FROM edges e
		JOIN node_props_text ap ON ap.node_id = e.%[1]s
		JOIN property_keys kap ON kap.id = ap.key_id AND kap.key = 'path'
		JOIN node_props_text ab ON ab.node_id = e.%[1]s
		JOIN property_keys kab ON kab.id = ab.key_id AND kab.key = 'blob_hash'
		JOIN node_props_text op ON op.node_id = e.%[2]s
		JOIN property_keys kop ON kop.id = op.key_id AND kop.key = 'path'
		LEFT JOIN property_keys kot ON kot.key = 'title'
		LEFT JOIN node_props_text ot ON ot.node_id = e.%[2]s AND ot.key_id = kot.id
		LEFT JOIN property_keys koty ON koty.key = 'type'
		LEFT JOIN node_props_text oty ON oty.node_id = e.%[2]s AND oty.key_id = koty.id
		LEFT JOIN property_keys kod ON kod.key = 'deleted'
		LEFT JOIN node_props_text od ON od.node_id = e.%[2]s AND od.key_id = kod.id
		LEFT JOIN property_keys kec ON kec.key = ?
		LEFT JOIN edge_props_text ec ON ec.edge_id = e.id AND ec.key_id = kec.id
		WHERE e.type = ? AND ap.value = ? AND ab.value = ?`, anchorCol, otherCol)

	rows, err := ex.QueryContext(ctx, q, commitKey, EdgeDerivedFrom, path, blobHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RefSummary
	for rows.Next() {
		var rs RefSummary
		var deleted string
		if err := rows.Scan(&rs.Path, &rs.Title, &rs.Type, &rs.Commit, &deleted); err != nil {
			return nil, err
		}
		if rs.Path == "" {
			continue
		}
		rs.Deleted = deleted == "true"
		out = append(out, rs)
	}
	return out, rows.Err()
}

// graphSimilarToNeighbours returns DISTINCT (a, b) path pairs joined by a
// SIMILAR_TO edge where `a` is one of anchorPaths.
//
// SIMILAR_TO is written DIRECTED but read UNDIRECTED (Cypher used the
// `-[:SIMILAR_TO]-` form). Both orientations are therefore unioned — querying a
// single direction would silently shrink every cluster.
//
// The far endpoint is always required to be live. requireAnchorLive additionally
// excludes soft-deleted anchors, matching subgraph edge reads; the cohesion
// reader does not filter its anchors. A node with no `deleted` property counts
// as live (the property defaults to false).
//
// The traversal is ANCHOR-DRIVEN: the anchor set is resolved first through
// idx_node_props_text_key_value, then each orientation joins `edges` through
// idx_edges_source / idx_edges_target. Unioning the two orientations of the
// WHOLE edge table first and filtering afterwards is equivalent but forces a
// full type-scan plus an automatic-index build per chunk, independent of how
// few anchors were asked for.
func graphSimilarToNeighbours(ctx context.Context, ex storegit.CtxExecer, anchorPaths []string, requireAnchorLive bool) ([][2]string, error) {
	if len(anchorPaths) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(anchorPaths)), ",")

	anchorLive := ""
	if requireAnchorLive {
		anchorLive = `
		  AND NOT EXISTS (
			SELECT 1 FROM node_props_text ad
			JOIN property_keys kad ON kad.id = ad.key_id AND kad.key = 'deleted'
			WHERE ad.node_id = i.a_id AND ad.value = 'true'
		  )`
	}

	q := fmt.Sprintf(`
		WITH anchors AS (
			SELECT ap.node_id AS a_id, ap.value AS a_path
			FROM node_props_text ap
			JOIN property_keys kap ON kap.id = ap.key_id AND kap.key = 'path'
			WHERE ap.value IN (%s)
		),
		incident AS (
			SELECT an.a_id, an.a_path, e.target_id AS b_id
			FROM anchors an
			JOIN edges e ON e.source_id = an.a_id AND e.type = ?
			UNION ALL
			SELECT an.a_id, an.a_path, e.source_id AS b_id
			FROM anchors an
			JOIN edges e ON e.target_id = an.a_id AND e.type = ?
		)
		SELECT DISTINCT i.a_path, bp.value
		FROM incident i
		JOIN property_keys kbp ON kbp.key = 'path'
		JOIN node_props_text bp ON bp.node_id = i.b_id AND bp.key_id = kbp.id
		WHERE NOT EXISTS (
			SELECT 1 FROM node_props_text bd
			JOIN property_keys kbd ON kbd.id = bd.key_id AND kbd.key = 'deleted'
			WHERE bd.node_id = i.b_id AND bd.value = 'true'
		  )%s`, placeholders, anchorLive)

	// Argument order follows the statement text: the anchor IN-list is bound in
	// the first CTE, then one edge-type parameter per orientation.
	args := make([]any, 0, len(anchorPaths)+2)
	for _, p := range anchorPaths {
		args = append(args, p)
	}
	args = append(args, EdgeSimilarTo, EdgeSimilarTo)

	rows, err := ex.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, b})
	}
	return out, rows.Err()
}

// graphDetachDeleteNode removes a node and everything incident to it.
// Equivalent to cypher DETACH DELETE n. Labels, properties and edges (both
// directions) go via ON DELETE CASCADE, which the connection enables with
// _foreign_keys=1; edge properties cascade in turn from edges.
func graphDetachDeleteNode(ctx context.Context, ex storegit.CtxExecer, nodeID int64) error {
	if _, err := ex.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, nodeID); err != nil {
		return fmt.Errorf("graphDetachDeleteNode: %w", err)
	}
	return nil
}
