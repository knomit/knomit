package graph

import (
	"context"
	"knomit/internal/corpus/synthesize/internal/synthtest"
	"knomit/internal/corpus/synthesize/step"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScopedCluster_GroupsBySubgraphEdges drives ScopedCluster through the
// subgraph-local clustering path: with no extra neighbors, the subgraph is the
// seed set, and the SIMILAR_TO edges returned by SubgraphEdges decide the
// grouping. The connected pair forms a cluster; the isolated seed falls below
// minCommunitySize and is dropped.
func TestScopedCluster_GroupsBySubgraphEdges(t *testing.T) {
	ctx := context.Background()
	branch := "agent/x"

	seeds := []step.FactForLLM{
		{File: "kb/a.md", Title: "A"},
		{File: "kb/b.md", Title: "B"},
		{File: "kb/c.md", Title: "C"},
	}

	// No neighbors: the subgraph is exactly the seed set. a–b are similar (a
	// real SIMILAR_TO edge); c is isolated.
	env := synthtest.NewGraphEnv(t, branch).SeedAll(seeds).Similar([2]string{"kb/a.md", "kb/b.md"})
	idx := env.Idx()
	reads := synthtest.GuardReads(t, idx, synthtest.ReadPolicy{Allow: []string{"Search", "SubgraphEdges"}})

	clusters, err := ScopedCluster(ctx, seeds, idx, 1.0, 2, synthtest.DefaultNeighborKinds, nil, branch)
	require.NoError(t, err)

	require.Len(t, clusters, 1, "exactly one cluster survives minCommunitySize=2")
	files := []string{clusters[0][0].File, clusters[0][1].File}
	require.ElementsMatch(t, []string{"kb/a.md", "kb/b.md"}, files)
	require.Equal(t, 1, reads.CountOf("SubgraphEdges"), "the edge read happens once")
}
