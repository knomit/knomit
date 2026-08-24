package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/synthesize"
)

// The trap this closes, stated once: a bare test RepoInstance leaves
// MaxMembers at ZERO, and a zero member cap gates out every bridge candidate.
// A measurement tool configured that way reports "0 near, 0 far" on every
// corpus, and the zeros read as a finding about the corpora rather than about
// the harness. rulings-3 caught it in the test harness; this tool reproduced it
// on its first run.

func TestProductionQuality_IsNotTheGateEverythingOutDefault(t *testing.T) {
	q := productionQuality()
	require.NotNil(t, q)
	require.Positive(t, q.MaxMembers,
		"a zero member cap gates out EVERY bridge candidate; the tool would measure an engine that cannot emit")
}

// Read, never re-typed. The Phase-3 review noted the test harness's copies of
// these values could drift from the real defaults with only a sibling test
// pinning them. A measurement tool that drifts from production configuration
// measures a different engine, quietly — so this asserts identity with the
// shipped defaults rather than with a second list of numbers.
func TestProductionQuality_MatchesTheShippedDefaultsExactly(t *testing.T) {
	d := config.Defaults().Discovery
	q := productionQuality()

	require.Equal(t, d.CohFloor, q.CohFloor)
	require.Equal(t, d.QualityFloor, q.QualityFloor)
	require.Equal(t, d.WCoh, q.WCoh)
	require.Equal(t, d.WGap, q.WGap)
	require.Equal(t, d.WSpec, q.WSpec)
	require.Equal(t, d.MaxMembers, q.MaxMembers)
}

// And the thing the two above are really protecting: what the engine is handed.
// Asserted at the boundary the scorer actually reads (lesson 8) rather than at
// the struct the tool happens to build.
func TestProductionQuality_ReachesTheScorerAsProductionValues(t *testing.T) {
	d := config.Defaults().Discovery
	ri := testInstanceWithQuality(t)

	got := synthesize.QualityConfigFromRepo(ri)

	require.Equal(t, d.MaxMembers, got.MaxMembers)
	require.Equal(t, d.CohFloor, got.CohFloor)
	require.Positive(t, got.MaxMembers, "precondition: production's cap is non-zero, or this test proves nothing")
}

// testInstanceWithQuality builds the same RepoInstance shape open() builds,
// minus the store and embedder, so the quality knobs can be asserted where the
// scorer reads them without opening a corpus.
func testInstanceWithQuality(t *testing.T) *repos.RepoInstance {
	t.Helper()
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "lab", AgentBranch: "main", OntologyRoot: "kb",
		Quality: productionQuality(),
	})
}
