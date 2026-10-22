package store

import "knomit/internal/engine/store/index"

// The index child's unexported graph primitives and constants under the
// names this package's white-box tests used before the move. Each function is
// the child's *ForTest seam (panics outside a test binary); the constants are
// the child's values, re-read here.
var (
	graphNodeIDByProps       = index.GraphNodeIDByPropsForTest
	graphMergeNode           = index.GraphMergeNodeForTest
	graphSetNodeProps        = index.GraphSetNodePropsForTest
	graphMergeEdge           = index.GraphMergeEdgeForTest
	graphDeleteOutgoingEdges = index.GraphDeleteOutgoingEdgesForTest
	graphDeleteIncomingEdges = index.GraphDeleteIncomingEdgesForTest
	graphSimilarToNeighbours = index.GraphSimilarToNeighboursForTest
	graphDetachDeleteNode    = index.GraphDetachDeleteNodeForTest
	schemaVersionKey         = index.SchemaVersionKeyForTest
)

const (
	knnK          = index.KNNKForTest
	factsVecTable = index.FactsVecTableForTest

	// The names (*index.Index).SchemaVersionStateForTest returns.
	schemaCurrent = "current"
	schemaMissing = "missing"
	schemaStale   = "stale"
)
