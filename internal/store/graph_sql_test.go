package store

import (
	"context"
	"path/filepath"
	"sync"
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

// TestGraphMergeEdge_IsAtomicUnderConcurrency asserts the contract behind the
// single-statement form: the exists-check and the insert have to evaluate
// together, or two writers can both observe "absent" before either inserts.
//
// ux_edges_merge_identity (migration 000015) is the backstop, and
// TestEdges_MergeIdentityIndexIsEnforced covers it. The index turns a split
// two-statement form into a constraint error rather than a silent duplicate —
// which is why this test asserts NoError as well as the row count. Concurrent
// MERGE must succeed, not merely avoid duplicating.
//
// HONEST LIMIT: this does NOT reliably fail against the two-statement form.
// Both statements complete in microseconds, so in-process the window rarely
// opens. It was verified by widening it — a 1ms sleep between the SELECT and
// the INSERT turns this into 13 duplicate rows before the index existed, and
// into constraint failures after. Treat it as a contract assertion, not proof
// the race is gone.
func TestGraphMergeEdge_IsAtomicUnderConcurrency(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	a, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "a", "blob_hash": "1"})
	require.NoError(t, err)
	b, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "b", "blob_hash": "2"})
	require.NoError(t, err)

	// Hammer the same (src, tgt, type) from many goroutines on the bare pool.
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	start := make(chan struct{})
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximise overlap
			if err := graphMergeEdge(ctx, db, a, b, EdgeSimilarTo); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeSimilarTo),
		"concurrent merges of the same edge must collapse to exactly one row")
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

// ── graphSimilarToNeighbours ─────────────────────────────────────────────────

// TestGraphSimilarToNeighbours_ReadsBothOrientations is the discriminating test
// for the undirected read, and the reason it has to live at this level.
//
// SIMILAR_TO is written DIRECTED but read UNDIRECTED. Neither SubgraphEdges nor
// SimilarityAdjacency can catch a regression here: both pass their ENTIRE path
// set as anchors, so a direction-blind reader still finds every intra-set edge
// from its other endpoint, and the callers symmetrise the result anyway. Only a
// query whose anchor set is a strict subset — asking from the TARGET side of a
// directed edge — can tell the two apart. Dropping the reverse arm of the union
// silently shrinks every cluster; this is what fails when that happens.
func TestGraphSimilarToNeighbours_ReadsBothOrientations(t *testing.T) {
	si, ctx := newGraphTestIndex(t)

	a := mergeFactNode(t, si, "kb/a.md", "aaaa")
	b := mergeFactNode(t, si, "kb/b.md", "bbbb")

	// One DIRECTED edge, a → b. Nothing points back at a.
	require.NoError(t, graphMergeEdge(ctx, si.rh.db, a, b, EdgeSimilarTo))

	// Forward: anchored on the SOURCE. A directed-only reader also passes this.
	fwd, err := graphSimilarToNeighbours(ctx, si.rh.db, []string{"kb/a.md"}, false)
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"kb/a.md", "kb/b.md"}}, fwd)

	// Reverse: anchored on the TARGET only. This is the assertion that fails
	// if the reader stops unioning both orientations.
	rev, err := graphSimilarToNeighbours(ctx, si.rh.db, []string{"kb/b.md"}, false)
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"kb/b.md", "kb/a.md"}}, rev,
		"a SIMILAR_TO edge must be visible from its target, not just its source")
}

// TestGraphSimilarToNeighbours_FiltersDeletedEndpoints pins which endpoint each
// caller filters. The far endpoint is ALWAYS required to be live; the anchor is
// filtered only when requireAnchorLive is set (SubgraphEdges does, the cohesion
// reader does not). Reads of `deleted` and writes of it have to move together —
// porting one without the other mis-filters silently.
func TestGraphSimilarToNeighbours_FiltersDeletedEndpoints(t *testing.T) {
	si, ctx := newGraphTestIndex(t)

	a := mergeFactNode(t, si, "kb/a.md", "aaaa")
	b := mergeFactNode(t, si, "kb/b.md", "bbbb")
	require.NoError(t, graphMergeEdge(ctx, si.rh.db, a, b, EdgeSimilarTo))

	// Soft-delete the FAR endpoint (b), anchored on a. Excluded either way.
	require.NoError(t, graphSetNodeProps(ctx, si.rh.db, b, map[string]string{"deleted": "true"}))
	for _, requireAnchorLive := range []bool{false, true} {
		got, err := graphSimilarToNeighbours(ctx, si.rh.db, []string{"kb/a.md"}, requireAnchorLive)
		require.NoError(t, err)
		require.Empty(t, got, "a soft-deleted far endpoint is always excluded")
	}

	// Now the mirror: b live again, a soft-deleted, and anchor on a — so the
	// deleted node is the ANCHOR and the live one is the far endpoint. This is
	// the only configuration where requireAnchorLive changes the answer.
	require.NoError(t, graphSetNodeProps(ctx, si.rh.db, b, map[string]string{"deleted": "false"}))
	require.NoError(t, graphSetNodeProps(ctx, si.rh.db, a, map[string]string{"deleted": "true"}))

	kept, err := graphSimilarToNeighbours(ctx, si.rh.db, []string{"kb/a.md"}, false)
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"kb/a.md", "kb/b.md"}}, kept,
		"requireAnchorLive=false must not filter a soft-deleted anchor (the cohesion reader)")

	dropped, err := graphSimilarToNeighbours(ctx, si.rh.db, []string{"kb/a.md"}, true)
	require.NoError(t, err)
	require.Empty(t, dropped,
		"requireAnchorLive=true excludes a soft-deleted anchor (SubgraphEdges)")
}

// TestGraphSimilarToNeighbours_TreatsMissingDeletedPropAsLive pins the default:
// a node that never had `deleted` written counts as live. Historical nodes and
// fixtures both rely on this — an inner join on the property would drop them.
func TestGraphSimilarToNeighbours_TreatsMissingDeletedPropAsLive(t *testing.T) {
	si, ctx := newGraphTestIndex(t)

	// No `deleted` property on either node.
	a, err := graphMergeNode(ctx, si.rh.db, NodeFact,
		map[string]string{"path": "kb/a.md", "blob_hash": "aaaa"})
	require.NoError(t, err)
	b, err := graphMergeNode(ctx, si.rh.db, NodeFact,
		map[string]string{"path": "kb/b.md", "blob_hash": "bbbb"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, si.rh.db, a, b, EdgeSimilarTo))

	got, err := graphSimilarToNeighbours(ctx, si.rh.db, []string{"kb/a.md"}, true)
	require.NoError(t, err)
	require.Equal(t, [][2]string{{"kb/a.md", "kb/b.md"}}, got,
		"an absent `deleted` property means live")
}

// ── gcOrphanedGraphNodes ─────────────────────────────────────────────────────

// TestGCOrphanedGraphNodes_CollectsEntityOrphans covers the latent bug this
// change fixes. The Cypher version projected n.path and skipped rows where it
// was empty — but Entity nodes are keyed by `name` and have NO path, so entity
// orphans were never actually collected. Matching by node id fixes it, which
// means GC starts deleting rows it previously left behind: worth pinning.
func TestGCOrphanedGraphNodes_CollectsEntityOrphans(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	fact := mergeFactNode(t, si, "kb/a.md", "aaaa")

	tagged, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "kept"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, fact, tagged, EdgeTagged))

	orphan, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "orphan"})
	require.NoError(t, err)

	require.Equal(t, 2, countNodesWithLabel(t, si, ctx, NodeEntity))
	require.NoError(t, si.gcOrphanedGraphNodes(ctx, NodeEntity, EdgeTagged))

	require.Equal(t, 1, countNodesWithLabel(t, si, ctx, NodeEntity),
		"the untagged Entity must be collected")

	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_labels WHERE node_id = ?`, tagged).Scan(&n))
	require.Equal(t, 1, n, "the tagged Entity survives")

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_props_text WHERE node_id = ?`, orphan).Scan(&n))
	require.Equal(t, 0, n, "the collected orphan's properties cascade")

	// The Fact node itself is never touched by entity GC.
	require.Equal(t, 1, countNodesWithLabel(t, si, ctx, NodeFact))
	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeTagged))
}

// TestGCOrphanedGraphNodes_RequiresIncomingEdgeFromFact pins the source-label
// half of the predicate: an incoming edge of the right TYPE from a node that is
// not a Fact does not save an orphan.
func TestGCOrphanedGraphNodes_RequiresIncomingEdgeFromFact(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	// A Domain node pointed at by another Domain, not by a Fact.
	parent, err := graphMergeNode(ctx, db, NodeDomain, map[string]string{"path": "tech"})
	require.NoError(t, err)
	child, err := graphMergeNode(ctx, db, NodeDomain, map[string]string{"path": "tech/go"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, child, parent, EdgeDomainChildOf))
	// Wrong source label for the IN_DOMAIN predicate.
	require.NoError(t, graphMergeEdge(ctx, db, child, parent, EdgeInDomain))

	require.NoError(t, si.gcOrphanedGraphNodes(ctx, NodeDomain, EdgeInDomain))
	require.Equal(t, 0, countNodesWithLabel(t, si, ctx, NodeDomain),
		"only an IN_DOMAIN edge FROM a Fact keeps a Domain alive")
}

// TestGCOrphanedGraphNodes_KeepsReferencedDomain is the positive case: a Domain
// with a real IN_DOMAIN edge from a Fact survives GC.
func TestGCOrphanedGraphNodes_KeepsReferencedDomain(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	fact := mergeFactNode(t, si, "kb/a.md", "aaaa")
	dom, err := graphMergeNode(ctx, db, NodeDomain, map[string]string{"path": "tech/go"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, fact, dom, EdgeInDomain))

	require.NoError(t, si.gcOrphanedGraphNodes(ctx, NodeDomain, EdgeInDomain))
	require.Equal(t, 1, countNodesWithLabel(t, si, ctx, NodeDomain))
	require.Equal(t, 1, countEdgesOfType(t, si, ctx, EdgeInDomain))
}

// TestGCOrphanedGraphNodes_NoOrphansIsANoOp guards the set-based DELETE against
// over-reach: with every node referenced, GC must delete nothing at all.
func TestGCOrphanedGraphNodes_NoOrphansIsANoOp(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	fact := mergeFactNode(t, si, "kb/a.md", "aaaa")
	ent, err := graphMergeNode(ctx, db, NodeEntity, map[string]string{"name": "kept"})
	require.NoError(t, err)
	require.NoError(t, graphMergeEdge(ctx, db, fact, ent, EdgeTagged))

	var before int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&before))
	require.NoError(t, si.gcOrphanedGraphNodes(ctx, NodeEntity, EdgeTagged))

	var after int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&after))
	require.Equal(t, before, after, "GC with no orphans must not delete anything")
}

// The merge-identity index is what makes duplicate relationship edges
// impossible rather than merely unlikely. DERIVED_FROM must stay exempt: it is
// a deliberate multi-edge, one row per ref-event.
func TestEdges_MergeIdentityIndexIsEnforced(t *testing.T) {
	si, ctx := newGraphTestIndex(t)
	db := si.rh.db

	a, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "a", "blob_hash": "1"})
	require.NoError(t, err)
	b, err := graphMergeNode(ctx, db, NodeFact, map[string]string{"path": "b", "blob_hash": "2"})
	require.NoError(t, err)

	// A raw duplicate insert of a relationship edge must be rejected by the DB,
	// not merely avoided by application code.
	_, err = db.ExecContext(ctx,
		`INSERT INTO edges(source_id, target_id, type) VALUES (?, ?, ?)`, a, b, EdgeSimilarTo)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO edges(source_id, target_id, type) VALUES (?, ?, ?)`, a, b, EdgeSimilarTo)
	require.Error(t, err, "a duplicate SIMILAR_TO edge must violate ux_edges_merge_identity")

	// DERIVED_FROM is exempt — multiple lineage edges per (src,tgt) are correct.
	for range 3 {
		_, err = db.ExecContext(ctx,
			`INSERT INTO edges(source_id, target_id, type) VALUES (?, ?, ?)`, a, b, EdgeDerivedFrom)
		require.NoError(t, err, "DERIVED_FROM must remain a multi-edge")
	}
	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE type = ?`, EdgeDerivedFrom).Scan(&n))
	require.Equal(t, 3, n)
}
