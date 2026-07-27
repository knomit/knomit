package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGraphSchema_OwnedByMigration pins the property-graph schema now that
// knomit declares it outright.
//
// These tables used to appear as a side effect of the GraphQLite extension's
// first cypher() call (migration 000003 was `SELECT cypher('RETURN 1')`). With
// the extension gone, migration 000014 is the only thing that creates them — if
// it regressed, a fresh database would come up with no graph at all.
//
// It also asserts the typed property tables stay dropped: every property is
// stored as TEXT, because no graph read does a numeric or range comparison.
// Re-introducing them would mean writes had silently started splitting values
// across tables the readers never consult.
func TestGraphSchema_OwnedByMigration(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()

	ctx := context.Background()
	exists := func(table string) bool {
		var n int
		require.NoError(t, svc.si.rh.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table).Scan(&n))
		return n > 0
	}

	for _, table := range []string{
		"nodes", "node_labels", "edges",
		"property_keys", "node_props_text", "edge_props_text",
	} {
		require.True(t, exists(table),
			"migration 000014 must create %q on a fresh database", table)
	}

	for _, table := range []string{
		"node_props_int", "node_props_real", "node_props_bool", "node_props_json",
		"edge_props_int", "edge_props_real", "edge_props_bool", "edge_props_json",
	} {
		require.False(t, exists(table),
			"typed property table %q must stay dropped — all properties are TEXT", table)
	}
}

// TestGraphSchema_CascadesFromNodes pins the FK cascade the delete paths rely
// on. graphDetachDeleteNode and gcOrphanedGraphNodes both reduce Cypher's
// DETACH DELETE to a plain DELETE FROM nodes, which is only correct while the
// schema cascades labels, properties and incident edges — and while the
// connection actually enforces foreign keys (_foreign_keys=1 on the DSN).
func TestGraphSchema_CascadesFromNodes(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	a, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "a", "blob_hash": "1"})
	require.NoError(t, err)
	b, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "b", "blob_hash": "2"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, a, b, EdgeSimilarTo))

	count := func(q string, args ...any) int {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, q, args...).Scan(&n))
		return n
	}
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM edges WHERE source_id = ?`, a))

	_, err = db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, a)
	require.NoError(t, err)

	require.Equal(t, 0, count(`SELECT COUNT(*) FROM node_labels WHERE node_id = ?`, a), "labels must cascade")
	require.Equal(t, 0, count(`SELECT COUNT(*) FROM node_props_text WHERE node_id = ?`, a), "properties must cascade")
	require.Equal(t, 0, count(`SELECT COUNT(*) FROM edges WHERE source_id = ? OR target_id = ?`, a, a),
		"incident edges must cascade in both directions")
	require.Equal(t, 1, count(`SELECT COUNT(*) FROM node_labels WHERE node_id = ?`, b), "unrelated node survives")
}
