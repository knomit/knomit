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
// vocabularies hold "155 clusters". They hold 156 — 37 + 71 + 26 + 22.
//
// WHERE THE 155 CAME FROM (corrected; the first version of this comment got
// its own arithmetic wrong and said "37 + 71 + 26 + 20", which is 154, not the
// 155 that was actually published). The 155 is 37 + **70** + 26 + 22: merged's
// RAW-SQL cluster count, not the shipped VocabularyHealth figure of 71. That
// is precisely the discrepancy the T0-T2 record documented and resolved in
// favour of the shipped API — "correct instrument, wrong habit" — and it then
// leaked into a sibling record's total anyway.
//
// Pinned by a test for the same reason the gate annex's 7→8 correction was:
// the T0-T2 record's own words are that "a number nobody re-derived is exactly
// how the annex's own §11 item 4 happened". Which applies to the first draft of
// this very comment: a causal story nobody re-derived, inside the test built
// against that failure mode. The 133 definitions figure was always right
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
	// The published 155 is reproducible from the raw-SQL merged count, which is
	// what makes the causal story above checkable rather than asserted.
	rawSQLMerged := map[string]int{} // a COPY: assigning the map would alias it
	for k, v := range clusters {
		rawSQLMerged[k] = v
	}
	rawSQLMerged["merged"] = 70
	require.Equal(t, 155, sumOf(rawSQLMerged),
		"the published 155 is the shipped total with merged's raw-SQL count substituted")
	require.Equal(t, 156, sumOf(clusters), "and the original is untouched by that substitution")
	require.NotEqual(t, sumOf(clusters), sumOf(definitions),
		"precondition: cluster and definition totals differ, or this test could not "+
			"tell one from the other")
}

func sumOf(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
