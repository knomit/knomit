package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The LLM layer of alias resolution. A judge merge asserts that two clusters
// the mechanical layer kept apart name the SAME mechanism.
//
// Merges are persisted and OVERLAID onto each mechanical rebuild rather than
// recomputed: re-judging the whole vocabulary every session would turn §3.1's
// "one bounded prompt" into a cost that grows with the corpus.

func TestMotifJudge_MergeUnifiesTwoMechanicalClusters(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "quiet-degradation")
	require.NoError(t, err)
	require.NotEqual(t, a, b, "precondition: the mechanical layer must keep these apart")

	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "same mechanism under two names"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err = svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err = svc.Motifs().CanonicalID(ctx, branch, "quiet-degradation")
	require.NoError(t, err)
	require.Equal(t, a, b, "a judge merge must unify the two clusters")
}

// The property that makes the judge pass incremental instead of per-session:
// a merge recorded once survives later rebuilds it never re-authorised.
func TestMotifJudge_MergeSurvivesLaterRebuilds(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "same mechanism under two names"))

	// Three more rebuilds, and a corpus change in between.
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	b, err := svc.Motifs().CanonicalID(ctx, branch, "quiet-degradation")
	require.NoError(t, err)
	require.Equal(t, a, b,
		"a judge decision must not need re-authorising every session — that is what "+
			"makes the pass incremental rather than a per-session re-judging of the corpus")

	// ...and it did not drag an unrelated cluster in with it.
	c, err := svc.Motifs().CanonicalID(ctx, branch, "config-drift")
	require.NoError(t, err)
	require.NotEqual(t, a, c)
}

// A merge whose vocabulary has left the corpus must go inert without anything
// cleaning it up, and must not resurrect a retired spelling.
func TestMotifJudge_MergeGoesInertWhenItsVocabularyVanishes(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "same mechanism under two names"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	// The second mechanism leaves the corpus entirely.
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	tbl, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.NotContains(t, tbl, "quiet-degradation",
		"a stale judge merge must not resurrect a spelling no live fact carries")
	require.Contains(t, tbl, "silent-fallback")
	require.Contains(t, tbl, "config-drift")

	// The assertion above is NOT enough, and the gap is instructive: alias rows
	// are keyed by live spellings, so the dead one cannot appear there whatever
	// the overlay does. That made the test pass with the liveness check
	// deleted, while a real bug went through.
	//
	// The bug: union() with a dead endpoint still parents the LIVE key to the
	// DEAD one whenever the dead key sorts first, so the surviving cluster ends
	// up identified by a key no live spelling has. Measured — without the
	// check, "silent-fallback" comes back keyed "degradation-quiet".
	//
	// That is not cosmetic. Cluster keys are what T2's definitions hang off, so
	// a cluster keyed to departed vocabulary would lose its definition to a
	// retirement rather than to a change in meaning.
	key, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.Equal(t, groupingKey("silent-fallback"), key,
		"a surviving cluster must be identified by a LIVE key, never by the key of "+
			"vocabulary that has left the corpus")
}

// Merges are transitive: a judge that merges A-B and B-C has said all three
// name one mechanism, and df counted over anything less would be wrong.
func TestMotifJudge_MergesAreTransitive(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	writeMotifFact(t, svc, branch, "kb/alpha/three.md", []string{"mute-failover"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "same mechanism under two names"))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch, "quiet-degradation", "mute-failover", "same mechanism under two names"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	c, err := svc.Motifs().CanonicalID(ctx, branch, "mute-failover")
	require.NoError(t, err)
	require.Equal(t, a, c, "A~B and B~C must place A and C in one cluster")

	// And df sees all three carriers as one mechanism.
	df, err := svc.Search().TokenDF(ctx, branch, a, "motif")
	require.NoError(t, err)
	require.Equal(t, 3, df)
}

// MN3 again, now with the LLM layer involved: judge merges must not rewrite
// facts either.
func TestMotifJudge_MN3_MergeTouchesNoFact(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/alpha/one.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/alpha/two.md", []string{"quiet-degradation"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	before := map[string]string{}
	for _, p := range []string{"kb/alpha/one.md", "kb/alpha/two.md"} {
		rec, err := svc.FactQuery().GetByPath(ctx, branch, p)
		require.NoError(t, err)
		before[p] = rec.BlobHash
	}

	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch, "silent-fallback", "quiet-degradation", "same mechanism under two names"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	for p, want := range before {
		rec, err := svc.FactQuery().GetByPath(ctx, branch, p)
		require.NoError(t, err)
		require.Equal(t, want, rec.BlobHash,
			"MN3: a judge merge must never rewrite a fact — %s changed", p)
	}
}

// A judge chain must survive the RETIREMENT of its middle.
//
// A~B and B~C is an assertion that all three name ONE mechanism. Retiring B's
// spelling withdraws a word from the corpus, not a judgement about mechanisms —
// so A and C must stay together, with nothing left recording the judge's view
// if they do not.
//
// The earlier implementation skipped any merge whose endpoints were not both
// live, which split the chain silently.
func TestMotifJudge_ChainSurvivesTheRetirementOfItsMiddle(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	writeMotifFact(t, svc, branch, "kb/a.md", []string{"silent-fallback"})
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"quiet-degradation"})
	writeMotifFact(t, svc, branch, "kb/c.md", []string{"mute-failover"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"silent-fallback", "quiet-degradation", "one mechanism"))
	require.NoError(t, svc.Motifs().RecordJudgeMerge(ctx, branch,
		"quiet-degradation", "mute-failover", "the same one"))
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err := svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	c, err := svc.Motifs().CanonicalID(ctx, branch, "mute-failover")
	require.NoError(t, err)
	require.Equal(t, a, c, "precondition: the chain joins all three")

	// The MIDDLE spelling leaves the corpus entirely.
	writeMotifFact(t, svc, branch, "kb/b.md", []string{"config-drift"})
	require.NoError(t, svc.Motifs().RebuildAliases(ctx, branch))

	a, err = svc.Motifs().CanonicalID(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	c, err = svc.Motifs().CanonicalID(ctx, branch, "mute-failover")
	require.NoError(t, err)
	require.Equal(t, a, c,
		"A and C must stay one cluster: the judge asserted they name one mechanism, "+
			"and retiring the spelling that linked them withdraws a word, not a judgement")

	// ...and the surviving key must be one a LIVE spelling has, not the
	// retired middle's.
	key, err := svc.Motifs().ClusterKey(ctx, branch, "silent-fallback")
	require.NoError(t, err)
	require.Contains(t, []string{groupingKey("silent-fallback"), groupingKey("mute-failover")}, key,
		"the surviving cluster must be keyed by live vocabulary")

	// The retired spelling itself is gone.
	tbl, err := svc.Motifs().AliasTable(ctx, branch)
	require.NoError(t, err)
	require.NotContains(t, tbl, "quiet-degradation")
}
