package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// These pin the direct-SQL graph write primitives that replace the cypher()
// write path. The critical property is MERGE semantics: Rebuild never wipes
// the graph (there is no DELETE FROM nodes/edges anywhere), so it re-runs the
// same writes on every rebuild. A blind INSERT would duplicate nodes and edges
// on each pass; these primitives must be idempotent by identity.

func newGraphTestIndex(t *testing.T) (*searchIndex, context.Context) {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc.si, context.Background()
}

// countNodesWithLabel counts nodes carrying the given label.
func countNodesWithLabel(t *testing.T, si *searchIndex, ctx context.Context, label string) int {
	t.Helper()
	var n int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_labels WHERE label = ?`, label).Scan(&n))
	return n
}

func countEdgesOfType(t *testing.T, si *searchIndex, ctx context.Context, edgeType string) int {
	t.Helper()
	var n int
	require.NoError(t, si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, edgeType).Scan(&n))
	return n
}

// TestGraphMergeNode_IsIdempotentByIdentity is the load-bearing one: merging
// the same (label, identity props) twice must return the same node id and
// create exactly one node — otherwise every Rebuild duplicates the graph.
func TestGraphMergeNode_IsIdempotentByIdentity(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	id1, err := graphMergeNode(ctx, db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "deadbeef"})
	require.NoError(t, err)
	require.NotZero(t, id1)

	id2, err := graphMergeNode(ctx, db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "deadbeef"})
	require.NoError(t, err)
	require.Equal(t, id1, id2, "MERGE must return the existing node, not create a second")
	require.Equal(t, 1, countNodesWithLabel(t, si, ctx, NodeFact))

	// A different identity is a different node (Fact identity is per-version).
	id3, err := graphMergeNode(ctx, db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "cafebabe"})
	require.NoError(t, err)
	require.NotEqual(t, id1, id3, "a different blob_hash is a distinct Fact version")
	require.Equal(t, 2, countNodesWithLabel(t, si, ctx, NodeFact))

	// Same identity values but a different label must not collide.
	idEnt, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "kb/a.md"})
	require.NoError(t, err)
	require.NotEqual(t, id1, idEnt)
	require.Equal(t, 1, countNodesWithLabel(t, si, ctx, NodeEntity))
}

// TestGraphMergeNode_IdentityPropsAreReadable verifies the merged node's
// identity props land in node_props_text so graphNodeIDByBlob-style lookups
// (and the ported readers) can find them.
func TestGraphMergeNode_IdentityPropsAreReadable(t *testing.T) {
	si, ctx := newGraphTestIndex(t)

	id, err := graphMergeNode(ctx, si.rh.db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "deadbeef"})
	require.NoError(t, err)

	got, err := si.graphNodeIDByBlob(ctx, "kb/a.md", "deadbeef")
	require.NoError(t, err)
	require.Equal(t, id, got, "identity props must be stored in node_props_text")
}

func TestGraphSetNodeProps_InsertsAndOverwrites(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	id, err := graphMergeNode(ctx, db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "deadbeef"})
	require.NoError(t, err)

	require.NoError(t, graphSetNodeProps(ctx, db, id,
		map[string]string{"title": "first", "deleted": "false"}))

	readProp := func(key string) string {
		var v string
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT p.value FROM node_props_text p
			JOIN property_keys k ON k.id = p.key_id AND k.key = ?
			WHERE p.node_id = ?`, key, id).Scan(&v))
		return v
	}
	require.Equal(t, "first", readProp("title"))
	require.Equal(t, "false", readProp("deleted"))

	// Overwrite must replace, not duplicate (PK is (node_id, key_id)).
	require.NoError(t, graphSetNodeProps(ctx, db, id,
		map[string]string{"title": "second", "deleted": "true"}))
	require.Equal(t, "second", readProp("title"))
	require.Equal(t, "true", readProp("deleted"))

	var propRows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_props_text WHERE node_id = ?`, id).Scan(&propRows))
	require.Equal(t, 4, propRows, "path+blob_hash identity props plus title+deleted, no duplicates")
}

func TestGraphMergeEdge_IsIdempotent(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	a, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "a", "blob_hash": "1"})
	require.NoError(t, err)
	b, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "ent"})
	require.NoError(t, err)

	require.NoError(t, graphMergeEdge(ctx, db, a, b, EdgeTagged))
	require.NoError(t, graphMergeEdge(ctx, db, a, b, EdgeTagged))
	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeTagged),
		"MERGE on an existing edge must not create a duplicate")
}

func TestGraphDeleteEdges_ByDirectionAndType(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	a, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "a", "blob_hash": "1"})
	require.NoError(t, err)
	b, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "b", "blob_hash": "2"})
	require.NoError(t, err)
	ent, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "ent"})
	require.NoError(t, err)

	require.NoError(t, graphMergeEdge(ctx, db, a, ent, EdgeTagged))
	require.NoError(t, graphMergeEdge(ctx, db, a, b, EdgeSimilarTo))
	require.NoError(t, graphMergeEdge(ctx, db, b, a, EdgeSimilarTo))

	// Outgoing-only, type-scoped: a-[SIMILAR_TO]->b goes, b-[SIMILAR_TO]->a stays,
	// and a-[TAGGED]->ent is untouched.
	require.NoError(t, graphDeleteOutgoingEdges(ctx, db, a, EdgeSimilarTo))
	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeSimilarTo))
	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeTagged))

	// Incoming-only, type-scoped: removes b-[SIMILAR_TO]->a.
	require.NoError(t, graphDeleteIncomingEdges(ctx, db, a, EdgeSimilarTo))
	require.Equal(t, 0, countEdgesOfType(t, si, ctx, EdgeSimilarTo))
	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeTagged))
}

// TestGraphDetachDeleteNode_CascadesEdgesAndProps replaces cypher's
// DETACH DELETE. FK cascade (_foreign_keys=1) should remove labels, props and
// edges in both directions.
func TestGraphDetachDeleteNode_CascadesEdgesAndProps(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	a, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "a", "blob_hash": "1"})
	require.NoError(t, err)
	ent, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "ent"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, a, ent, EdgeTagged))
	require.NoError(t, graphSetNodeProps(ctx, db, ent, map[string]string{"extra": "x"}))

	require.NoError(t, graphDetachDeleteNode(ctx, db, ent))

	require.Equal(t, 0, countNodesWithLabel(t, si, ctx, NodeEntity), "label rows must cascade")
	require.Equal(t, 0, countEdgesOfType(t, si, ctx, EdgeTagged), "incident edges must cascade")

	var props int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_props_text WHERE node_id = ?`, ent).Scan(&props))
	require.Equal(t, 0, props, "props must cascade")

	// The other node survives untouched.
	require.Equal(t, 1, countNodesWithLabel(t, si, ctx, NodeFact))
}
