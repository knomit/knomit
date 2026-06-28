package synthesize

import (
	"context"

	"go.uber.org/mock/gomock"

	"knomit/internal/store"
)

// expectScopedClusterPartition wires a MockSearchIndex so ScopedCluster
// reproduces the given community partition in-process, replacing the old
// CachedClusterFacts mock seam now that clustering is scoped+in-process.
//
// Neighbor searches (Search with QueryByPath set) return no extra neighbors, so
// the clustered subgraph is exactly the seed set. SubgraphEdges returns a star
// of edges WITHIN each community and none across, so gonum Louvain recovers
// `comms` as separate connected components. Facts meant to be in distinct
// communities are given no connecting edge; bridgePathCommunities resolves such
// facts to distinct synthetic ids even when filterSmallClusters drops them as
// sub-floor singletons — so the partition holds for any minCommunitySize.
//
// pool feeds the pool-load Search (no QueryByPath) that BridgeComponentReport
// issues; pass nil for callers like BuildBackwardBridges that receive their seed
// pool directly and never issue a pool-load Search.
func expectScopedClusterPartition(idx *MockSearchIndex, pool []store.SearchResult, comms [][]string) {
	idx.EXPECT().Search(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, opts store.SearchOptions) ([]store.SearchResult, error) {
			if opts.QueryByPath != "" {
				return nil, nil // neighbor search: subgraph stays the seed set
			}
			return pool, nil // pool-load search
		}).AnyTimes()
	idx.EXPECT().SubgraphEdges(gomock.Any(), gomock.Any()).
		Return(intraCommunityEdges(comms), nil).AnyTimes()
}

// intraCommunityEdges returns a star of SIMILAR_TO edges within each community
// (first member linked to the rest) and no cross-community edge, so Louvain
// recovers exactly the given partition. Singleton communities contribute no
// edge.
func intraCommunityEdges(comms [][]string) [][2]string {
	var edges [][2]string
	for _, c := range comms {
		for i := 1; i < len(c); i++ {
			edges = append(edges, [2]string{c[0], c[i]})
		}
	}
	return edges
}
