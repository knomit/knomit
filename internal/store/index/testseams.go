package index

import (
	"context"
	"fmt"
	"testing"

	"knomit/internal/store/facts"
	storegit "knomit/internal/store/git"
	"knomit/internal/store/internal/vcs"
)

// Test seams for the parent's white-box tests, which drive the index through
// a full store.Service and then reach into its unexported cores. Each panics
// outside a test binary (testing.Testing), so no production path can use the
// door. Every seam is the named core with nothing added.

func mustBeTest(name string) {
	if !testing.Testing() {
		panic("index." + name + " called outside a test binary")
	}
}

// RepoForTest is the *vcs.Repo the index reaches up to (the parent's rh.Repo).
func (si *Index) RepoForTest() *vcs.Repo { mustBeTest("RepoForTest"); return si.rh }

// SetBeforeSimTxHookForTest installs f to run in rebuildGraph after the KNN
// pass and before the similarity transaction opens; nil removes it.
func (si *Index) SetBeforeSimTxHookForTest(f func()) {
	mustBeTest("SetBeforeSimTxHookForTest")
	si.beforeSimTx = f
}

// SetInSimTxHookForTest installs f to run inside the similarity transaction,
// after the prunes and before the merge loop; nil removes it.
func (si *Index) SetInSimTxHookForTest(f func()) {
	mustBeTest("SetInSimTxHookForTest")
	si.inSimTx = f
}

// SetInSimEdgeTxHookForTest installs f to run inside graphBuildSimilarityEdges'
// transaction, after the version's delete-outgoing and before its re-merge;
// nil removes it.
func (si *Index) SetInSimEdgeTxHookForTest(f func()) {
	mustBeTest("SetInSimEdgeTxHookForTest")
	si.inSimEdgeTx = f
}

// SetBeforeGraphTxHookForTest installs f to run in rebuildGraph after the
// git-read cache is built and before its write transaction opens; nil removes
// it.
func (si *Index) SetBeforeGraphTxHookForTest(f func()) {
	mustBeTest("SetBeforeGraphTxHookForTest")
	si.beforeGraphTx = f
}

// SetSubgraphEdgeChunkSizeForTest sets the chunk size SubgraphEdges binds
// per query and returns the previous value, for the caller to restore.
func SetSubgraphEdgeChunkSizeForTest(n int) int {
	mustBeTest("SetSubgraphEdgeChunkSizeForTest")
	prev := subgraphEdgeChunkSize
	subgraphEdgeChunkSize = n
	return prev
}

// SchemaVersionStateForTest is schemaVersionState, named: "current",
// "missing" or "stale".
func (si *Index) SchemaVersionStateForTest(ctx context.Context, branch string) (string, error) {
	mustBeTest("SchemaVersionStateForTest")
	st, err := si.schemaVersionState(ctx, branch)
	switch st {
	case schemaCurrent:
		return "current", err
	case schemaMissing:
		return "missing", err
	case schemaStale:
		return "stale", err
	}
	return fmt.Sprintf("unknown(%d)", st), err
}

// SchemaVersionKeyForTest is schemaVersionKey.
func SchemaVersionKeyForTest(branch string) string {
	mustBeTest("SchemaVersionKeyForTest")
	return schemaVersionKey(branch)
}

// KNNKForTest is knnK, the top-K nearest neighbours per fact.
const KNNKForTest = knnK

// FactsVecTableForTest is the name of the code-managed facts vector table.
const FactsVecTableForTest = factsVecTable

// UpsertForTest is upsert.
func (si *Index) UpsertForTest(ctx context.Context, branch, commitHash string, rec facts.FactRecord) error {
	mustBeTest("UpsertForTest")
	return si.upsert(ctx, branch, commitHash, rec)
}

// EnsureFactsVecForTest is ensureFactsVec.
func (si *Index) EnsureFactsVecForTest(ctx context.Context, modelID string, dim int) error {
	mustBeTest("EnsureFactsVecForTest")
	return si.ensureFactsVec(ctx, modelID, dim)
}

// VecTableExistsForTest is vecTableExists.
func (si *Index) VecTableExistsForTest(ctx context.Context, table string) (bool, error) {
	mustBeTest("VecTableExistsForTest")
	return si.vecTableExists(ctx, table)
}

// GetEmbeddingByFactForTest is getEmbeddingByFact.
func (si *Index) GetEmbeddingByFactForTest(ctx context.Context, path, blobHash string) ([]float32, error) {
	mustBeTest("GetEmbeddingByFactForTest")
	return si.getEmbeddingByFact(ctx, path, blobHash)
}

// RebuildEmbeddingsForTest is rebuildEmbeddings.
func (si *Index) RebuildEmbeddingsForTest(ctx context.Context, progress vcs.RebuildProgress) (int, error) {
	mustBeTest("RebuildEmbeddingsForTest")
	return si.rebuildEmbeddings(ctx, progress)
}

// RebuildGraphForTest is rebuildGraph.
func (si *Index) RebuildGraphForTest(ctx context.Context, branch string, progress vcs.RebuildProgress) (int, error) {
	mustBeTest("RebuildGraphForTest")
	return si.rebuildGraph(ctx, branch, progress)
}

// GCOrphanedGraphNodesForTest is gcOrphanedGraphNodes.
func (si *Index) GCOrphanedGraphNodesForTest(ctx context.Context, label, edgeType string) error {
	mustBeTest("GCOrphanedGraphNodesForTest")
	return si.gcOrphanedGraphNodes(ctx, label, edgeType)
}

// ResolveTargetCommitForTest is resolveTargetCommit.
func (si *Index) ResolveTargetCommitForTest(ctx context.Context, branch, sourcePath, refPath, sourceCommit string) (string, bool, error) {
	mustBeTest("ResolveTargetCommitForTest")
	return si.resolveTargetCommit(ctx, branch, sourcePath, refPath, sourceCommit)
}

// GraphAddDerivedFromAtCommitTxForTest is graphAddDerivedFromAtCommitTx.
func (si *Index) GraphAddDerivedFromAtCommitTxForTest(ctx context.Context, tx storegit.CtxExecer, branch, sourcePath, sourceBlobHash, sourceCommit string, refs []string) error {
	mustBeTest("GraphAddDerivedFromAtCommitTxForTest")
	return si.graphAddDerivedFromAtCommitTx(ctx, tx, branch, sourcePath, sourceBlobHash, sourceCommit, refs)
}

// GraphNodeIDByBlobForTest is graphNodeIDByBlob.
func (si *Index) GraphNodeIDByBlobForTest(ctx context.Context, path, blobHash string) (int64, error) {
	mustBeTest("GraphNodeIDByBlobForTest")
	return si.graphNodeIDByBlob(ctx, path, blobHash)
}

// GraphInsertEdgeForTest is graphInsertEdge.
func (si *Index) GraphInsertEdgeForTest(ctx context.Context, sourceID, targetID int64, edgeType string) error {
	mustBeTest("GraphInsertEdgeForTest")
	return si.graphInsertEdge(ctx, sourceID, targetID, edgeType)
}

// GraphInsertEdgeReturningIDForTest is graphInsertEdgeReturningID.
func (si *Index) GraphInsertEdgeReturningIDForTest(ctx context.Context, sourceID, targetID int64, edgeType string) (int64, error) {
	mustBeTest("GraphInsertEdgeReturningIDForTest")
	return si.graphInsertEdgeReturningID(ctx, sourceID, targetID, edgeType)
}

// GraphSetEdgePropsForTest is graphSetEdgeProps.
func (si *Index) GraphSetEdgePropsForTest(ctx context.Context, edgeID int64, props map[string]string) error {
	mustBeTest("GraphSetEdgePropsForTest")
	return si.graphSetEdgeProps(ctx, edgeID, props)
}

// GraphBuildSimilarityEdgesForTest is graphBuildSimilarityEdges.
func (si *Index) GraphBuildSimilarityEdgesForTest(ctx context.Context, path, blobHash string) error {
	mustBeTest("GraphBuildSimilarityEdgesForTest")
	return si.graphBuildSimilarityEdges(ctx, path, blobHash)
}

// ReverseDependentPathsForTest is reverseDependentPaths.
func (gs *GraphStore) ReverseDependentPathsForTest(ctx context.Context, path string) (map[string]struct{}, error) {
	mustBeTest("ReverseDependentPathsForTest")
	return gs.reverseDependentPaths(ctx, path)
}

// GraphNodeIDByPropsForTest is graphNodeIDByProps.
func GraphNodeIDByPropsForTest(ctx context.Context, ex storegit.CtxExecer, label string, identity map[string]string) (int64, error) {
	mustBeTest("GraphNodeIDByPropsForTest")
	return graphNodeIDByProps(ctx, ex, label, identity)
}

// GraphMergeNodeForTest is graphMergeNode.
func GraphMergeNodeForTest(ctx context.Context, ex storegit.CtxExecer, label string, identity map[string]string) (int64, error) {
	mustBeTest("GraphMergeNodeForTest")
	return graphMergeNode(ctx, ex, label, identity)
}

// GraphSetNodePropsForTest is graphSetNodeProps.
func GraphSetNodePropsForTest(ctx context.Context, ex storegit.CtxExecer, nodeID int64, props map[string]string) error {
	mustBeTest("GraphSetNodePropsForTest")
	return graphSetNodeProps(ctx, ex, nodeID, props)
}

// GraphMergeEdgeForTest is graphMergeEdge.
func GraphMergeEdgeForTest(ctx context.Context, ex storegit.CtxExecer, srcID, tgtID int64, edgeType string) error {
	mustBeTest("GraphMergeEdgeForTest")
	return graphMergeEdge(ctx, ex, srcID, tgtID, edgeType)
}

// GraphDeleteOutgoingEdgesForTest is graphDeleteOutgoingEdges.
func GraphDeleteOutgoingEdgesForTest(ctx context.Context, ex storegit.CtxExecer, nodeID int64, edgeType string) error {
	mustBeTest("GraphDeleteOutgoingEdgesForTest")
	return graphDeleteOutgoingEdges(ctx, ex, nodeID, edgeType)
}

// GraphDeleteIncomingEdgesForTest is graphDeleteIncomingEdges.
func GraphDeleteIncomingEdgesForTest(ctx context.Context, ex storegit.CtxExecer, nodeID int64, edgeType string) error {
	mustBeTest("GraphDeleteIncomingEdgesForTest")
	return graphDeleteIncomingEdges(ctx, ex, nodeID, edgeType)
}

// GraphSimilarToNeighboursForTest is graphSimilarToNeighbours.
func GraphSimilarToNeighboursForTest(ctx context.Context, ex storegit.CtxExecer, anchorPaths []string, requireAnchorLive bool) ([][2]string, error) {
	mustBeTest("GraphSimilarToNeighboursForTest")
	return graphSimilarToNeighbours(ctx, ex, anchorPaths, requireAnchorLive)
}

// GraphDetachDeleteNodeForTest is graphDetachDeleteNode.
func GraphDetachDeleteNodeForTest(ctx context.Context, ex storegit.CtxExecer, nodeID int64) error {
	mustBeTest("GraphDetachDeleteNodeForTest")
	return graphDetachDeleteNode(ctx, ex, nodeID)
}
