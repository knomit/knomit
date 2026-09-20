package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// writeFactOn is writeTestFact against an explicit branch.
func writeFactOn(t *testing.T, svc *store.Service, branch, path, title string, typ fact.Type) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = "body of " + title
	f.Type = typ
	f.Confidence = 0.8
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestExperiment_ReviewSeedsOnlyTheDelta is the payoff of copying the parent's
// pipeline_watermarks on fork: a review inside a fresh experiment seeds the
// facts added SINCE the fork, not the whole corpus.
//
// Three things make this falsifiable rather than decorative:
//
//   - The run is UNSCOPED. A scoped run is watermark-exempt and always
//     full-scans (decisions/architecture/synthesize/scope-filter), so a scoped
//     version of this test would pass whether or not the watermark was
//     inherited.
//   - It asserts the seed COUNT equals the delta, and separately that the
//     delta is strictly smaller than the corpus — without the second
//     assertion a full scan satisfies the first whenever they coincide.
//   - It asserts the scan took the INCREMENTAL path. A full scan that happened
//     to match the same count would otherwise read as success.
func TestExperiment_ReviewSeedsOnlyTheDelta(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()
	agent := ri.AgentBranch()

	// A corpus the parent has already reviewed: four facts, watermark at HEAD.
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		writeFactOn(t, svc, agent, "kb/observations/"+name+".md", name, fact.Observation)
	}
	agentHead, err := svc.Branches().HeadCommit(ctx, agent)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, reviewTool, agent, agentHead))

	exp, err := svc.Experiments().OpenExperiment(ctx, "delta-only", "", agent)
	require.NoError(t, err)

	// Exactly one new fact on the experiment.
	writeFactOn(t, svc, exp.Branch(), "kb/observations/epsilon.md", "epsilon", fact.Observation)

	corpus, err := svc.FactQuery().LiveFactCount(ctx, exp.Branch())
	require.NoError(t, err)
	require.Equal(t, 5, corpus, "precondition: the experiment sees the four inherited facts plus its own")

	p := NewReviewerOnBranch(ri, nil, EffortNormal, ScopeFilter{}, exp.Branch()).p
	seeds, scan, err := p.dirtyFacts(ctx, exp.Branch(), svc.Facts(), svc.Search(), svc.Pipeline())
	require.NoError(t, err)

	require.Equal(t, seedScanIncremental, scan.Path,
		"the inherited watermark must put the experiment on the incremental path, not a full scan")
	require.Equal(t, agentHead, scan.Watermark, "and the baseline is the parent's watermark, unchanged")
	require.Len(t, seeds, 1, "exactly the one fact added since the fork")
	require.Less(t, len(seeds), corpus, "and strictly fewer than the corpus, or a full scan would pass too")
	require.Equal(t, "kb/observations/epsilon.md", seeds[0].Path())
}

// TestExperiment_ReviewWithoutInheritedWatermarkFullScans is the control: the
// same fixture, with the experiment's inherited watermark removed, takes the
// full-scan path and seeds the whole corpus. It is what shows the assertion
// above is measuring the inheritance and not something incidental.
func TestExperiment_ReviewWithoutInheritedWatermarkFullScans(t *testing.T) {
	svc, ri := newHypothesizeTestRepo(t)
	ctx := context.Background()
	agent := ri.AgentBranch()

	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		writeFactOn(t, svc, agent, "kb/observations/"+name+".md", name, fact.Observation)
	}
	agentHead, err := svc.Branches().HeadCommit(ctx, agent)
	require.NoError(t, err)
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, reviewTool, agent, agentHead))

	exp, err := svc.Experiments().OpenExperiment(ctx, "no-watermark", "", agent)
	require.NoError(t, err)
	writeFactOn(t, svc, exp.Branch(), "kb/observations/epsilon.md", "epsilon", fact.Observation)

	// Undo the inheritance the fork performed.
	require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, reviewTool, exp.Branch(), ""))

	p := NewReviewerOnBranch(ri, nil, EffortNormal, ScopeFilter{}, exp.Branch()).p
	seeds, scan, err := p.dirtyFacts(ctx, exp.Branch(), svc.Facts(), svc.Search(), svc.Pipeline())
	require.NoError(t, err)

	require.Equal(t, seedScanFull, scan.Path, "no watermark means first-run-on-this-branch: full scan")
	require.Len(t, seeds, 5, "and the whole corpus is dirty — the cost the inheritance avoids")
}
