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
