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
// MERGE SEMANTICS ARE LOAD-BEARING. Rebuild never wipes the graph — there is
// no DELETE FROM nodes/edges anywhere — so it re-runs the same writes on every
// pass. graphMergeNode/graphMergeEdge must therefore be idempotent by identity;
// a blind INSERT would duplicate the whole graph on each rebuild.
//
// All properties are stored as TEXT in {node,edge}_props_text. The typed prop
// tables the GraphQLite extension used (_int/_real/_bool/_json) are not written:
// every graph read is projection or equality only, and the sole non-text
// predicate (deleted) is satisfied by comparing against 'true'/'false'.

// graphPropKeyID ensures a property_keys row exists for key and returns its id.
func graphPropKeyID(ctx context.Context, ex storegit.CtxExecer, key string) (int64, error) {
	if _, err := ex.ExecContext(ctx,
		`INSERT OR IGNORE INTO property_keys(key) VALUES (?)`, key); err != nil {
		return 0, fmt.Errorf("graphPropKeyID: ensure %q: %w", key, err)
	}
	var id int64
	if err := ex.QueryRowContext(ctx,
		`SELECT id FROM property_keys WHERE key = ?`, key).Scan(&id); err != nil {
		return 0, fmt.Errorf("graphPropKeyID: lookup %q: %w", key, err)
	}
	return id, nil
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
	for _, key := range sortedKeys(props) {
		keyID, err := graphPropKeyID(ctx, ex, key)
		if err != nil {
			return fmt.Errorf("graphSetNodeProps: %w", err)
		}
		if _, err := ex.ExecContext(ctx,
			`INSERT OR REPLACE INTO node_props_text(node_id, key_id, value) VALUES (?, ?, ?)`,
			nodeID, keyID, props[key]); err != nil {
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
func graphMergeEdge(ctx context.Context, ex storegit.CtxExecer, srcID, tgtID int64, edgeType string) error {
	var n int
	if err := ex.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE source_id = ? AND target_id = ? AND type = ? LIMIT 1`,
		srcID, tgtID, edgeType).Scan(&n); err != nil {
		return fmt.Errorf("graphMergeEdge(%s): dedup check: %w", edgeType, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO edges(source_id, target_id, type) VALUES (?, ?, ?)`,
		srcID, tgtID, edgeType); err != nil {
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
func graphSimilarToNeighbours(ctx context.Context, ex storegit.CtxExecer, anchorPaths []string, requireAnchorLive bool) ([][2]string, error) {
	if len(anchorPaths) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(anchorPaths)), ",")

	anchorLive := ""
	if requireAnchorLive {
		anchorLive = ` AND (ad.value IS NULL OR ad.value != 'true')`
	}

	q := fmt.Sprintf(`
		WITH undirected AS (
			SELECT source_id AS a_id, target_id AS b_id FROM edges WHERE type = ?
			UNION ALL
			SELECT target_id AS a_id, source_id AS b_id FROM edges WHERE type = ?
		)
		SELECT DISTINCT ap.value, bp.value
		FROM undirected u
		JOIN property_keys kp ON kp.key = 'path'
		JOIN node_props_text ap ON ap.node_id = u.a_id AND ap.key_id = kp.id
		JOIN node_props_text bp ON bp.node_id = u.b_id AND bp.key_id = kp.id
		LEFT JOIN property_keys kd ON kd.key = 'deleted'
		LEFT JOIN node_props_text ad ON ad.node_id = u.a_id AND ad.key_id = kd.id
		LEFT JOIN node_props_text bd ON bd.node_id = u.b_id AND bd.key_id = kd.id
		WHERE ap.value IN (%s)
		  AND (bd.value IS NULL OR bd.value != 'true')%s`, placeholders, anchorLive)

	args := make([]any, 0, len(anchorPaths)+2)
	args = append(args, EdgeSimilarTo, EdgeSimilarTo)
	for _, p := range anchorPaths {
		args = append(args, p)
	}

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
