package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestDefaultAxis_ImpactWhenDistilledLayerSeparates: syntheses carry edges,
// observations carry none — the ratio is infinite, so impact discriminates.
func TestDefaultAxis_ImpactWhenDistilledLayerSeparates(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)
	require.Equal(t, AxisImpact, res.DefaultAxis)
}

// TestDefaultAxis_ConfidenceWhenGraphIsFlat exercises the RATIO branch, not
// the zero-top-layer early return.
//
// The top layer must have NON-ZERO out-degree, otherwise `topMean <= 0`
// short-circuits and the 3x threshold is never reached — the test would pass
// without separationThreshold existing at all.
//
// Fixture arithmetic: principles derive 1 each (topMean = 1.0); observations
// x and y derive 2 each while leaf1/leaf2 derive 0 (obsMean = 1.0). Ratio =
// 1.0, below 3.0 -> confidence. This mirrors knomit-kb, measured at 0.9x.
func TestDefaultAxis_ConfidenceWhenGraphIsFlat(t *testing.T) {
	const branch = "main"
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	// Two leaf observations for others to derive from.
	for _, p := range []string{"kb/o/leaf1.md", "kb/o/leaf2.md"} {
		_, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("leaf "+p, fact.Observation, 0.5, nil), "add "+p, "")
		require.NoError(t, err)
	}
	// Observations WITH edges — the flat-graph signature.
	for _, p := range []string{"kb/o/x.md", "kb/o/y.md"} {
		_, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("obs "+p, fact.Observation, 0.5,
				[]string{"kb/o/leaf1.md", "kb/o/leaf2.md"}), "add "+p, "")
		require.NoError(t, err)
	}
	// Top-layer facts with SOME edges — enough to clear the zero guard, not
	// enough to clear the 3x ratio.
	for _, p := range []string{"kb/p/a.md", "kb/p/b.md"} {
		_, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("principle "+p, fact.Principle, 0.9,
				[]string{"kb/o/leaf1.md"}), "add "+p, "")
		require.NoError(t, err)
	}

	res, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	require.Equal(t, AxisConfidence, res.DefaultAxis)
	// And the list must actually be confidence-ordered.
	require.NotEmpty(t, res.Highlights)
	require.Equal(t, 0.9, res.Highlights[0].Confidence)
}

// TestDefaultAxis_ZeroTopLayerShortCircuits covers the OTHER confidence path —
// a top layer with no edges at all — so the two returns are distinguishable.
func TestDefaultAxis_ZeroTopLayerShortCircuits(t *testing.T) {
	const branch = "main"
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/o/leaf.md",
		typedFactBody("leaf", fact.Observation, 0.5, nil), "add leaf", "")
	require.NoError(t, err)
	// Observations carry edges; the top layer carries none.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/o/x.md",
		typedFactBody("obs x", fact.Observation, 0.5,
			[]string{"kb/o/leaf.md"}), "add x", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/p/a.md",
		typedFactBody("principle a", fact.Principle, 0.9, nil), "add a", "")
	require.NoError(t, err)

	res, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	require.Equal(t, AxisConfidence, res.DefaultAxis)
}

// TestDefaultAxis_ImpactViaRatioNotShortCircuit exercises the TRUE side of the
// ratio branch — the one path none of the other tests reach. Every other test
// hits either the topMean<=0 short-circuit or the obsMean==0 short-circuit;
// this is the only one where obsMean is finite and strictly positive AND the
// ratio clears separationThreshold. Without it, flipping the comparator to
// <=, or inverting the operands to *obsMean / *topMean, leaves the whole
// suite green — the 3x threshold would be unguarded in the direction that
// matters.
//
// Fixture arithmetic: four leaf observations carry no refs and one carries a
// single ref, so obsMean = (0+0+0+0+1)/5 = 0.2. Two syntheses each derive
// from all four leaves, so topMean = (4+4)/2 = 4.0. Ratio = 4.0/0.2 = 20,
// well clear of the 3.0 threshold -> impact.
func TestDefaultAxis_ImpactViaRatioNotShortCircuit(t *testing.T) {
	const branch = "main"
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	leaves := []string{"kb/o/leaf1.md", "kb/o/leaf2.md", "kb/o/leaf3.md", "kb/o/leaf4.md"}
	for _, p := range leaves {
		_, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("leaf "+p, fact.Observation, 0.5, nil), "add "+p, "")
		require.NoError(t, err)
	}
	// One observation with a single ref -> obsMean = 0.2, not zero.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/o/obs5.md",
		typedFactBody("obs5", fact.Observation, 0.5, leaves[:1]), "add obs5", "")
	require.NoError(t, err)
	// Two syntheses, each deriving from all four leaves -> topMean = 4.0.
	for _, p := range []string{"kb/s/a.md", "kb/s/b.md"} {
		_, err := svc.Facts().WriteFact(ctx, branch, p,
			typedFactBody("synthesis "+p, fact.Synthesis, 0.9, leaves), "add "+p, "")
		require.NoError(t, err)
	}

	res, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	require.Equal(t, AxisImpact, res.DefaultAxis)
}

// TestDefaultAxis_IsRepoScopedNotPathScoped: the list is folder-scoped but the
// axis is not — a small folder must not flip the control mid-navigation.
func TestDefaultAxis_IsRepoScopedNotPathScoped(t *testing.T) {
	const branch = "main"
	svc := seedHighlightFixture(t, branch)
	ctx := context.Background()

	root, err := svc.FactQuery().Stats(ctx, branch, "", "")
	require.NoError(t, err)
	scoped, err := svc.FactQuery().Stats(ctx, branch, "kb/o/", "")
	require.NoError(t, err)
	require.Equal(t, root.DefaultAxis, scoped.DefaultAxis)
}

// TestDefaultAxis_EmptyRepoDefaultsToConfidence: no facts, no separation.
func TestDefaultAxis_EmptyRepoDefaultsToConfidence(t *testing.T) {
	const branch = "main"
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	res, err := svc.FactQuery().Stats(context.Background(), branch, "", "")
	require.NoError(t, err)
	require.Equal(t, AxisConfidence, res.DefaultAxis)
}
