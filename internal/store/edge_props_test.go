package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGraphSetEdgeProps_WritesAndReadsText creates two nodes and an edge,
// sets two text properties on the edge, then reads them back via Cypher.
func TestGraphSetEdgeProps_WritesAndReadsText(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	si := svc.si
	ctx := context.Background()

	// Two Fact nodes, distinct (path, blob_hash).
	srcID, err := graphMergeNode(ctx, si.rh.db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "aaaa"})
	require.NoError(t, err)
	require.NotZero(t, srcID)
	tgtID, err := graphMergeNode(ctx, si.rh.db, NodeFact,
		map[string]string{"path": "kb/b.md", "blob_hash": "bbbb"})
	require.NoError(t, err)
	require.NotZero(t, tgtID)

	edgeID, err := si.graphInsertEdgeReturningID(ctx, srcID, tgtID, EdgeDerivedFrom)
	require.NoError(t, err)
	require.NotZero(t, edgeID)

	require.NoError(t, si.graphSetEdgeProps(ctx, edgeID, map[string]string{
		"source_commit": "1234abc",
		"target_commit": "5678def",
	}))

	// Read back from the EAV tables and verify both properties round-trip.
	rows, err := si.rh.db.QueryContext(ctx, `
		SELECT sc.value, tc.value
		FROM edges e
		JOIN edge_props_text sc ON sc.edge_id = e.id
		JOIN property_keys ksc ON ksc.id = sc.key_id AND ksc.key = 'source_commit'
		JOIN edge_props_text tc ON tc.edge_id = e.id
		JOIN property_keys ktc ON ktc.id = tc.key_id AND ktc.key = 'target_commit'
		WHERE e.type = 'DERIVED_FROM'
	`)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	var sc, tc string
	require.NoError(t, rows.Scan(&sc, &tc))
	require.False(t, rows.Next(), "expected exactly one edge row")
	require.NoError(t, rows.Err())
	require.Equal(t, "1234abc", sc)
	require.Equal(t, "5678def", tc)
}
