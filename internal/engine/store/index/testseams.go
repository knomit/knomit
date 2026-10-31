package index

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/facts"
	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/internal/vcs"
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

// SetBeforeRefEdgesWriteHookForTest installs f to run inside the reference
// edge transaction, after the claim is written and before the edges are; an
// error aborts the transaction. nil removes it.
func (si *Index) SetBeforeRefEdgesWriteHookForTest(f func() error) {
	mustBeTest("SetBeforeRefEdgesWriteHookForTest")
	si.beforeRefEdgesWrite = f
}

// SetBeforeRefRowsWriteHookForTest installs f to run inside the reference
// edge transaction, after the claim and the edges are written and before the
// ref_edges rows are; an error aborts the transaction. nil removes it.
func (si *Index) SetBeforeRefRowsWriteHookForTest(f func() error) {
	mustBeTest("SetBeforeRefRowsWriteHookForTest")
	si.beforeRefRowsWrite = f
}

// SetRefEdgesChunkForTest sets how many blobs one RefEdgesOfBlobs statement
// binds and returns the previous value, for the caller to restore.
func SetRefEdgesChunkForTest(n int) int {
	mustBeTest("SetRefEdgesChunkForTest")
	old := refEdgesChunk
	refEdgesChunk = n
	return old
}

// OutgoingByEdgesForTest is OutgoingAtCommit reading the DERIVED_FROM edges
// (the read before ref_edges) instead of ref_edges: everything else, the
// anchor resolution and the filters, is the same code.
func (gs *GraphStore) OutgoingByEdgesForTest(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error) {
	mustBeTest("OutgoingByEdgesForTest")
	return gs.outgoingAtCommit(ctx, branch, path, commitHash, func(blobHash string) ([]RefSummary, error) {
		return graphDerivedFromNeighbours(ctx, conn(ctx, gs.rh.DB()), path, blobHash, false)
	})
}

// SetRootCommitForTest replaces the root-commit lookup localRepoID uses (nil
// restores rh.RootCommit) and forgets the cached repo id, so the next call
// resolves through f.
func (si *Index) SetRootCommitForTest(f func(ctx context.Context, branch string) (string, error)) {
	mustBeTest("SetRootCommitForTest")
	si.repoIDMu.Lock()
	defer si.repoIDMu.Unlock()
	si.rootCommit = f
	si.repoID = ""
}

// LocalRepoIDForTest is localRepoID.
func (si *Index) LocalRepoIDForTest(ctx context.Context, branch string) string {
	mustBeTest("LocalRepoIDForTest")
	return si.localRepoID(ctx, branch)
}

// IncomingByEdgesForTest is IncomingAtCommit reading the DERIVED_FROM edges
// instead of ref_edges; everything else is the same code.
func (gs *GraphStore) IncomingByEdgesForTest(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error) {
	mustBeTest("IncomingByEdgesForTest")
	return gs.incomingAtCommit(ctx, branch, path, commitHash, false)
}

// BlastRadiusByEdgesForTest is BlastRadius walking the DERIVED_FROM edges.
func (gs *GraphStore) BlastRadiusByEdgesForTest(ctx context.Context, branch, path string) (int, error) {
	mustBeTest("BlastRadiusByEdgesForTest")
	return gs.blastRadius(ctx, branch, path, false)
}

// ReverseDependentPathsByEdgesForTest is ReverseDependentPaths walking the
// DERIVED_FROM edges.
func (gs *GraphStore) ReverseDependentPathsByEdgesForTest(ctx context.Context, path string) (map[string]struct{}, error) {
	mustBeTest("ReverseDependentPathsByEdgesForTest")
	return gs.reverseDependentPaths(ctx, path, false)
}

// SetOutdegreeByEdgesForTest makes the highlights' out-degree read the
// DERIVED_FROM edges (true) or ref_edges (false).
func (fq *FactQuery) SetOutdegreeByEdgesForTest(byEdges bool) {
	mustBeTest("SetOutdegreeByEdgesForTest")
	fq.outdegreeByEdges.Store(byEdges)
}

// SetRootCommitForTest replaces the root-commit lookup of the fallback reads'
// repo id (nil restores rh.RootCommit).
func (gs *GraphStore) SetRootCommitForTest(f func(ctx context.Context, branch string) (string, error)) {
	mustBeTest("SetRootCommitForTest")
	if f == nil {
		gs.rootCommit.Store(nil)
		return
	}
	gs.rootCommit.Store(&f)
}

// ReverseDependentsPlanForTest is the EXPLAIN QUERY PLAN of the reverse walk
// (byEdges: the edge fallback walk; else the ref_edges walk).
func (gs *GraphStore) ReverseDependentsPlanForTest(ctx context.Context, byEdges bool) (string, error) {
	mustBeTest("ReverseDependentsPlanForTest")
	q, args := reverseDependentsByRowsSQL, []any{NodeFact, "kb/x.md", "kb/x.md"}
	if byEdges {
		q, args = reverseDependentsByEdgesSQL, []any{NodeFact, "kb/x.md", EdgeDerivedFrom, EdgeDerivedFrom, "kb/x.md"}
	}
	rows, err := conn(ctx, gs.rh.DB()).QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return "", err
		}
		out = append(out, detail)
	}
	return strings.Join(out, "\n"), rows.Err()
}

// RepairRefEdgesForTest is repairRefEdges.
func (si *Index) RepairRefEdgesForTest(ctx context.Context, branch string) (int, error) {
	mustBeTest("RepairRefEdgesForTest")
	return si.repairRefEdges(ctx, branch)
}

// SetBlobHoldingsChunkForTest sets how many pairs one blobHoldings statement
// binds and returns the previous value, for the caller to restore.
func SetBlobHoldingsChunkForTest(n int) int {
	mustBeTest("SetBlobHoldingsChunkForTest")
	prev := blobHoldingsChunk
	blobHoldingsChunk = n
	return prev
}

// IntroducingCommitForTest is introducingCommit.
func (si *Index) IntroducingCommitForTest(ctx context.Context, branch, path, blobHash, commit string) (string, error) {
	mustBeTest("IntroducingCommitForTest")
	return si.introducingCommit(ctx, branch, path, blobHash, commit)
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
func (si *Index) RebuildEmbeddingsForTest(ctx context.Context, progress model.RebuildProgress) (int, error) {
	mustBeTest("RebuildEmbeddingsForTest")
	return si.rebuildEmbeddings(ctx, progress)
}

// RebuildGraphForTest is rebuildGraph.
func (si *Index) RebuildGraphForTest(ctx context.Context, branch string, progress model.RebuildProgress) (int, error) {
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
	repoID := si.localRepoID(ctx, branch)
	return si.graphAddDerivedFromAtCommitTx(ctx, tx, branch, sourcePath, sourceBlobHash, sourceCommit, refSpecsOf(refs, repoID), repoID)
}

// CommitEarlierForTest is commitEarlier.
func (si *Index) CommitEarlierForTest(ctx context.Context, a, b string) (bool, error) {
	mustBeTest("CommitEarlierForTest")
	return si.commitEarlier(ctx, a, b)
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
	return gs.reverseDependentPaths(ctx, path, true)
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

// SearchFunc is the signature of FactQuery.Search.
type SearchFunc = func(ctx context.Context, branch string, q SearchOptions) ([]SearchResult, error)

// Overrides is the test-only way to observe, fail or fake the READ services of
// one store. It exists for two needs a seeded corpus cannot meet: proving a
// code path made NO read (or only the expected ones), and forcing a read to
// fail. It deliberately cannot supply canned answers for graph reads: a test
// that needs a similarity graph, a document frequency or a reverse-dependency
// set seeds real facts and graph edges instead.
//
//   - Reads is called with the method name at the top of every public read of
//     FactQuery, GraphStore, HistoryQuery and MethodologyMatcher (28 methods),
//     before any work. A test can record it, fail the test on an unexpected
//     one (strict mode), or return an error: the four reads that return an
//     error (SimilarityAdjacency, ReverseDependentPaths, TokenDF,
//     SubgraphEdges) return it to the caller; for every other read the return
//     value is ignored and the hook is an observer only.
//   - Search wraps FactQuery.Search: it receives the real method, so a test can
//     capture the context or substitute scored hits where no embedder exists.
//   - SubgraphEdges replaces the edge read ScopedCluster feeds to Louvain. It is
//     the one canned graph answer allowed, for a test that must fix a community
//     PARTITION while the same SIMILAR_TO table (read by SimilarityAdjacency for
//     cohesion) holds edges ACROSS those communities: one real table cannot be
//     both, so the clustering read is scripted and cohesion is seeded.
//
// NESTED CALLS: probes and the Search wrapper fire on nested calls too, not
// only on the call a test makes. ExplainFact calls IncomingAtCommit and
// OutgoingAtCommit (three probes for one call), and RecentFacts reaches
// FactQuery.Search through recentFactsSearch, so a Search wrapper also changes
// what RecentFacts returns. A strict policy must allow the nested names too.
//
// SCOPE: an Overrides is installed on the whole *Service's query services, not
// on one handle. Service.Search(), FactQuery(), GraphStore(), HistoryQuery()
// and Methodology() all hand out the same services, so installing through any
// of them affects every caller of the store, including other goroutines. Install
// it before concurrent use and restore it when the test ends.
type Overrides struct {
	Reads         func(method string) error
	Search        func(next SearchFunc) SearchFunc
	SubgraphEdges func(ctx context.Context, paths []string) ([][2]string, error)
}

// ReadName names a read SetReadFaultForTest can fail. It is a distinct type
// with one constant per faultable read, so a mistyped name is a compile error
// and not a fault that silently never fires.
type ReadName string

// The reads SetReadFaultForTest can fail.
const (
	ReadStats            ReadName = "Stats"
	ReadActivity         ReadName = "Activity"
	ReadCompletions      ReadName = "Completions"
	ReadRecentFacts      ReadName = "RecentFacts"
	ReadSearch           ReadName = "Search"
	ReadExplainFact      ReadName = "ExplainFact"
	ReadFactLiveAtCommit ReadName = "FactLiveAtCommit"
)

// ReadFault is the hook of SetReadFaultForTest: it is called with the name of a
// faultable read and returns the error that read must return, or nil to let it
// run.
type ReadFault func(read ReadName) error

// SetReadFaultForTest makes a read return fault(method) instead of running,
// when fault returns an error; a nil result lets the read through, and a nil
// fault removes the hook. It is FAIL-ONLY: it cannot fake an answer, and it is
// separate from Overrides, which stays an observer (Overrides.Reads errors on
// four reads only). The fault runs right after the Overrides probe, so an
// Overrides.Reads observer on the same Facade still sees the read, and may
// sleep there to model per-mount latency.
//
// Faultable reads: Stats, Activity, Completions, RecentFacts, Search,
// ExplainFact and FactLiveAtCommit. Nested calls fire it too, as the probes
// do: a Search fault also fails RecentFacts with Text set, which reaches
// Search. The hook is installed on this Facade's query services, that is on one
// store.Service; another Service is unaffected. It panics outside a test
// binary; the returned func restores the previous hook.
//
// The hook closure may run on several goroutines at once (the lens fan-out
// reads every mount concurrently), so a closure that records anything must be
// goroutine-safe. Match on the exact ReadName and assert the injected error's
// identity (a distinct sentinel or message), not only a status, so a test
// cannot pass for the wrong reason.
func (f *Facade) SetReadFaultForTest(fault ReadFault) (restore func()) {
	mustBeTest("SetReadFaultForTest")
	var p *ReadFault
	if fault != nil {
		p = &fault
	}
	prevF, prevG, prevH := f.FactQuery.fault.Load(), f.GraphStore.fault.Load(), f.HistoryQuery.fault.Load()
	f.FactQuery.fault.Store(p)
	f.GraphStore.fault.Store(p)
	f.HistoryQuery.fault.Store(p)
	return func() {
		f.FactQuery.fault.Store(prevF)
		f.GraphStore.fault.Store(prevG)
		f.HistoryQuery.fault.Store(prevH)
	}
}

// SetOverridesForTest installs o on all four query services of this Facade and
// returns a func that restores the previous set (see Overrides for the scope).
// It panics outside a test binary.
func (f *Facade) SetOverridesForTest(o Overrides) (restore func()) {
	mustBeTest("SetOverridesForTest")
	prevF, prevG, prevH, prevM := f.FactQuery.ov.Load(), f.GraphStore.ov.Load(), f.HistoryQuery.ov.Load(), f.MethodologyMatcher.ov.Load()
	f.FactQuery.ov.Store(&o)
	f.GraphStore.ov.Store(&o)
	f.HistoryQuery.ov.Store(&o)
	f.MethodologyMatcher.ov.Store(&o)
	return func() {
		f.FactQuery.ov.Store(prevF)
		f.GraphStore.ov.Store(prevG)
		f.HistoryQuery.ov.Store(prevH)
		f.MethodologyMatcher.ov.Store(prevM)
	}
}
