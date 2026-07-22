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
