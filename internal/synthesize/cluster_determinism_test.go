package synthesize

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

// ScopedCluster builds its node list by ranging over a map, and Go randomises
// map iteration order. Louvain IS seeded — the determinism was intended — but a
// fixed seed cannot help when the node ordering handed to Modularize changes
// every run, because its tie-breaking is order-dependent. So the same corpus
// clustered differently from one session to the next, and everything
// downstream (dedup, prune groups, distill groups, both bridge axes' Sep>=2
// gate) inherited the wobble.
//
// Measured before the fix: on the merged lab corpus a motif bridge candidate
// enumerated in four runs of five and vanished in the fifth.
//
// These two tests are the durable guarantee — the designer considered and
// declined a sorted-map container, on the grounds that the test is what holds
// the property, not the data structure.

const determinismSeedCount = 20

// The PROPERTY: the node ordering handed to Louvain is the same every run.
//
// Twenty paths over twenty runs. If the ordering were still drawn from map
// iteration, all twenty coincidences would need to happen at once, which does
// not occur. Stated as the property rather than as the mechanism, so a future
// implementation that achieves determinism some other way still passes.
func TestScopedCluster_HandsLouvainTheSameNodeOrderEveryRun(t *testing.T) {
	var seen [][]string
	for range determinismSeedCount {
		seen = append(seen, capturePathOrder(t))
	}

	// Precondition: enough nodes that a random order would differ — asserted rather
	// than assumed, because this test is worthless on a two-element slice.
	require.Len(t, seen[0], determinismSeedCount,
		"precondition: the subgraph must be large enough that a random order would differ")

	for i := 1; i < len(seen); i++ {
		require.Equalf(t, seen[0], seen[i],
			"run %d handed Louvain a different node order — the partition is not reproducible", i)
	}
}

// The MECHANISM, asserted separately because the property test above is
// probabilistic in principle and this one fails outright the moment the sort
// is removed. A check that can only fail by luck is not the whole guarantee.
func TestScopedCluster_NodeOrderIsSorted(t *testing.T) {
	got := capturePathOrder(t)
	require.True(t, slices.IsSorted(got),
		"the paths handed to SubgraphEdges must be in a fixed order; sorted is the one we chose")
}

// capturePathOrder runs ScopedCluster once and returns the path slice it handed
// to SubgraphEdges — the exact ordering that becomes Louvain's node numbering.
func capturePathOrder(t *testing.T) []string {
	t.Helper()
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	branch := "agent/x"

	seeds := make([]factForLLM, 0, determinismSeedCount)
	for i := range determinismSeedCount {
		// Names deliberately NOT in insertion order alphabetically, so a test
		// that passes cannot be passing because the map happened to be walked
		// in the order the slice was built.
		seeds = append(seeds, factForLLM{
			File:  fmt.Sprintf("kb/%02d-%c.md", determinismSeedCount-i, 'a'+rune(i%26)),
			Title: fmt.Sprintf("fact %d", i),
		})
	}

	idx.EXPECT().Search(gomock.Any(), branch, gomock.Any()).Return(nil, nil).AnyTimes()

	var captured []string
	idx.EXPECT().SubgraphEdges(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, paths []string) ([][2]string, error) {
			captured = append([]string(nil), paths...)
			return nil, nil
		}).Times(1)

	_, err := ScopedCluster(context.Background(), seeds, idx, 1.0, 2, nil, branch)
	require.NoError(t, err)
	require.NotEmpty(t, captured, "SubgraphEdges must have been called with the node list")
	return captured
}
