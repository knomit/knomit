package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// bridgeablePairs is the CEILING on motif bridging: the number of fact pairs
// that share a canonical motif at all. Nothing the engine does can exceed it,
// so it is the population the Phase-4 activation floor is set on (Q1 ruling).

func TestBridgeablePairs_HapaxCannotBridge(t *testing.T) {
	require.Equal(t, 0, bridgeablePairs(nil))
	require.Equal(t, 0, bridgeablePairs([]int{1, 1, 1, 1, 1}),
		"a motif on one fact joins nothing to anything")
	require.Equal(t, 0, bridgeablePairs([]int{0, 1}))
}

func TestBridgeablePairs_CountsUnorderedPairsPerCluster(t *testing.T) {
	require.Equal(t, 1, bridgeablePairs([]int{2}))
	require.Equal(t, 3, bridgeablePairs([]int{3}))
	require.Equal(t, 10, bridgeablePairs([]int{5}))
}

func TestBridgeablePairs_SumsAcrossClustersAndIgnoresHapax(t *testing.T) {
	// Deliberately non-degenerate: four distinct df values, two of them below
	// the band floor, and a total that no single term produces.
	got := bridgeablePairs([]int{1, 2, 3, 5})
	require.Equal(t, 0+1+3+10, got)
	require.NotEqual(t, 10, got, "precondition: the total must not equal its largest term")
}

// The published figure this reproduces: the gate annex says knomit-io-kb's
// whole saturated vocabulary offers TWO candidate pairs ("two shots is not a
// sample"). Its vocabulary is 22 clusters, 2 of them at df 2.
func TestBridgeablePairs_ReproducesTheAnnexKnomitIoKbFigure(t *testing.T) {
	dfs := make([]int, 0, 22)
	dfs = append(dfs, 2, 2)
	for len(dfs) < 22 {
		dfs = append(dfs, 1)
	}
	require.Equal(t, 2, bridgeablePairs(dfs))
}

// agentic-engineering's real df shape, read off the lab corpus 2026-08-24:
// one df-3 cluster and five df-2 clusters among 37. The annex §9 states this
// ceiling as 7; it is 8. Pinned here so the correction is executable rather
// than only written down.
func TestBridgeablePairs_AgenticEngineeringIsEightNotSeven(t *testing.T) {
	dfs := make([]int, 0, 37)
	dfs = append(dfs, 3, 2, 2, 2, 2, 2)
	for len(dfs) < 37 {
		dfs = append(dfs, 1)
	}
	require.Equal(t, 8, bridgeablePairs(dfs))
	require.NotEqual(t, 7, bridgeablePairs(dfs))
}

// L-1. The T7 record and the acceptance package both said the lab
// vocabularies hold "155 clusters". They hold 156: 37 + 71 + 26 + 20 is not
// the sum — it is 37 + 71 + 26 + 22, and the 22 is knomit-io-kb's cluster
// count, not its definition count (18).
//
// Pinned by a test for the same reason the gate annex's 7→8 correction was:
// the T0-T2 record's own words are that "a number nobody re-derived is
// exactly how the annex's own §11 item 4 happened", and that applies to its
// sibling record verbatim. The 133 definitions figure was always right
// (34+61+20+18).
func TestLabVocabularyTotals(t *testing.T) {
	clusters := map[string]int{
		"agentic-engineering": 37, "merged": 71, "knomit-kb": 26, "knomit-io-kb": 22,
	}
	definitions := map[string]int{
		"agentic-engineering": 34, "merged": 61, "knomit-kb": 20, "knomit-io-kb": 18,
	}
	require.Equal(t, 156, sumOf(clusters), "the T7/T8 cluster population is 156, not 155")
	require.Equal(t, 133, sumOf(definitions), "the real-definition count was always right")
	require.NotEqual(t, sumOf(clusters), sumOf(definitions),
		"precondition: the two totals differ, which is how 22 and 18 got swapped")
}

func sumOf(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
