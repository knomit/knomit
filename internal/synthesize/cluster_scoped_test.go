package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// TestScopedCluster_GroupsBySubgraphEdges drives ScopedCluster through the
// subgraph-local clustering path: with no extra neighbors, the subgraph is the
// seed set, and the SIMILAR_TO edges returned by SubgraphEdges decide the
// grouping. The connected pair forms a cluster; the isolated seed falls below
// minCommunitySize and is dropped.
func TestScopedCluster_GroupsBySubgraphEdges(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "agent/x"

	seeds := []factForLLM{
		{File: "kb/a.md", Title: "A"},
		{File: "kb/b.md", Title: "B"},
		{File: "kb/c.md", Title: "C"},
	}

	// No neighbors: the subgraph is exactly the seed set.
	idx.EXPECT().Search(gomock.Any(), branch, gomock.Any()).Return(nil, nil).AnyTimes()
	// a–b are similar; c is isolated.
	idx.EXPECT().SubgraphEdges(gomock.Any(), gomock.Any()).
		Return([][2]string{{"kb/a.md", "kb/b.md"}}, nil).Times(1)

	clusters, err := ScopedCluster(ctx, seeds, idx, 1.0, 2, nil, branch)
	require.NoError(t, err)

	require.Len(t, clusters, 1, "exactly one cluster survives minCommunitySize=2")
	files := []string{clusters[0][0].File, clusters[0][1].File}
	require.ElementsMatch(t, []string{"kb/a.md", "kb/b.md"}, files)
}
