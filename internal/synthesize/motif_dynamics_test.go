package synthesize

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Multi-session motif dynamics.
//
// The Phase-0 review's lesson, applied forward: every component here is
// trivially correct in isolation, and the failures live in what happens on the
// SECOND session, after an edit, or after a retraction. A test that calls a
// function once cannot see any of it.
//
// These assert through TokenDF rather than by reading the junction directly:
// df is the public surface Phase 1 actually ships, it is what later phases
// consume, and it is the number a wrong answer here would corrupt. Per-path
// junction correctness is asserted where the SQL lives, in
// internal/store/fact_motifs_test.go.
//
// Phase 1's named question — does a motif written in session N survive
// dedup-merge in session N+1 — is driven through the real knomit_learn handler
// in internal/mcp/learn_motif_test.go, because internal/mcp imports this
// package and the dependency cannot run the other way.

func motifDF(t *testing.T, e *restatementEnv, motif string) int {
	t.Helper()
	n, err := e.svc.Search().TokenDF(context.Background(), e.branch, motif, "motif")
	require.NoError(t, err)
	return n
}

// TestMotifDynamics_SurvivesEditAcrossSessions — facts rows are
// content-addressed, so an edit is a NEW row with a NEW id, and the junction is
// keyed by fact_id. If the new row's motifs were not re-derived, an edited fact
// would silently lose them: its df would drop by one with nothing to point at,
// and the loss would stay invisible until a much later phase went looking for a
// bridge whose seed had quietly evaporated.
func TestMotifDynamics_SurvivesEditAcrossSessions(t *testing.T) {
	env := newRestatementEnv(t, 0)

	// Session 1.
	env.writeFactWithMotifs("kb/alpha/one.md", "Title one", "body one",
		[]string{"silent-fallback"})
	require.Equal(t, 1, motifDF(t, env, "silent-fallback"))

	// Session 2: edit the body, keep the motif. The fact is a new row; the
	// motif must come with it.
	env.writeFactWithMotifs("kb/alpha/one.md", "Title one", "body one, revised",
		[]string{"silent-fallback"})
	require.Equal(t, 1, motifDF(t, env, "silent-fallback"),
		"an edit must neither drop the motif nor double-count it")

	// Session 3: change the motif itself.
	env.writeFactWithMotifs("kb/alpha/one.md", "Title one", "body one, revised again",
		[]string{"config-drift"})
	require.Equal(t, 1, motifDF(t, env, "config-drift"))
	require.Zero(t, motifDF(t, env, "silent-fallback"),
		"a motif removed by an edit must stop counting immediately — the superseded "+
			"revision is still in git, but df is about what is LIVE")
}

// TestMotifDynamics_DFTracksLivePathsNotRevisions — TokenDF counts DISTINCT
// live paths. Rewriting one fact three times must not inflate its motif's df to
// three: a later phase reads df as corroboration ("how many facts share this
// regularity"), and edit churn is not corroboration. Getting this wrong would
// promote a single much-edited fact into a df band it never earned.
func TestMotifDynamics_DFTracksLivePathsNotRevisions(t *testing.T) {
	env := newRestatementEnv(t, 0)
	for i := range 3 {
		env.writeFactWithMotifs("kb/alpha/one.md", "Title one",
			fmt.Sprintf("body revision %d", i), []string{"silent-fallback"})
	}
	env.writeFactWithMotifs("kb/alpha/two.md", "Title two", "body two",
		[]string{"silent-fallback"})

	require.Equal(t, 2, motifDF(t, env, "silent-fallback"),
		"df counts live paths, not the revisions behind them")
}

// TestMotifDynamics_RetractionRemovesTheMotif — deleting a fact must take its
// junction rows with it, or df keeps counting a carrier that no longer exists
// and every consumer inherits a phantom.
func TestMotifDynamics_RetractionRemovesTheMotif(t *testing.T) {
	env := newRestatementEnv(t, 0)
	ctx := context.Background()

	env.writeFactWithMotifs("kb/alpha/one.md", "Title one", "body one",
		[]string{"silent-fallback"})
	env.writeFactWithMotifs("kb/alpha/two.md", "Title two", "body two",
		[]string{"silent-fallback"})
	require.Equal(t, 2, motifDF(t, env, "silent-fallback"))

	_, err := env.svc.Facts().DeleteFact(ctx, env.branch, "kb/alpha/two.md", "retract two")
	require.NoError(t, err)

	require.Equal(t, 1, motifDF(t, env, "silent-fallback"),
		"a retracted fact must stop contributing to its motif's df")
}

// TestMotifDynamics_SubjectStripHoldsAcrossSessions — the strip is a write-time
// transform, so it must hold on EVERY write, not only the first. A strip that
// somehow ran once (a cached decision, a first-write-only code path) would let
// the same subject motif in on the second session, and nothing downstream would
// question it.
func TestMotifDynamics_SubjectStripHoldsAcrossSessions(t *testing.T) {
	env := newRestatementEnv(t, 0)

	// "widget-alpha" is entity ∪ domain for this helper's fixtures, so it is a
	// subject motif and never lands.
	env.writeFactWithMotifs("kb/alpha/one.md", "Title one", "body one",
		[]string{"widget-alpha", "silent-fallback"})
	require.Equal(t, 1, motifDF(t, env, "silent-fallback"))
	require.Zero(t, motifDF(t, env, "widget-alpha"))

	// Session 2: offered again, dropped again.
	env.writeFactWithMotifs("kb/alpha/one.md", "Title one", "body one, revised",
		[]string{"widget-alpha", "silent-fallback"})
	require.Equal(t, 1, motifDF(t, env, "silent-fallback"))
	require.Zero(t, motifDF(t, env, "widget-alpha"),
		"the strip must run on every write, not only the first")
}

// TestMotifDynamics_AccumulatesAcrossSessions — the ordinary case, and the one
// a later phase's df band depends on: motifs written by unrelated sessions over
// unrelated facts add up, and a motif nobody repeated stays at one.
func TestMotifDynamics_AccumulatesAcrossSessions(t *testing.T) {
	env := newRestatementEnv(t, 0)
	for i := range 5 {
		motifs := []string{"silent-fallback"}
		if i == 0 {
			motifs = append(motifs, "unmonitored-expiry")
		}
		env.writeFactWithMotifs(fmt.Sprintf("kb/alpha/f%d.md", i),
			fmt.Sprintf("Title %d", i), fmt.Sprintf("body %d", i), motifs)
	}

	require.Equal(t, 5, motifDF(t, env, "silent-fallback"))
	require.Equal(t, 1, motifDF(t, env, "unmonitored-expiry"),
		"a hapax motif stays a hapax — the band floor depends on this being honest")
}
