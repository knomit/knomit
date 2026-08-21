package synthesize

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEnsureTitleVectors_WatermarkIncremental is the conformance test for
// "a second review session on an unchanged corpus embeds ZERO titles, and a
// session after one fact edit embeds exactly one".
//
// Both are measured AFTER coverage completes, because a time-budgeted backfill
// legitimately continues across sessions until then — see
// TestEnsureTitleVectors_StopsAtTheLatencyBudget for that half.
func TestEnsureTitleVectors_WatermarkIncremental(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 3)

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Equal(t, 3, have)
	require.Equal(t, 3, total, "coverage completes in one session at this size")
	require.Equal(t, int64(3), env.emb.titles.Load())

	env.emb.titles.Store(0)
	have, _, err = ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Equal(t, 3, have)
	require.Equal(t, int64(0), env.emb.titles.Load(), "unchanged corpus embeds ZERO titles")

	env.writeFact("kb/f1.md", "F1 revised", "body-1-v2")

	env.emb.titles.Store(0)
	have, total, err = ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Equal(t, 3, have)
	require.Equal(t, 3, total)
	require.Equal(t, int64(1), env.emb.titles.Load(), "one edited fact embeds exactly one title")
}

// TestEnsureTitleVectors_UsesTheShortStringTemplate — MN9's guard on this side
// of the seam. Titles are short strings; embedding them as documents would put
// them through a rendering nothing in the motif work was calibrated under.
func TestEnsureTitleVectors_UsesTheShortStringTemplate(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 2)

	// Writing the facts embeds their bodies through the document path, which is
	// the write path's business — snapshot that counter so this test speaks
	// only about what the BACKFILL does.
	docsBefore := env.emb.documentCalls.Load()

	_, _, err := ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)

	require.Positive(t, env.emb.shortStringCalls.Load(), "titles embed as short strings")
	require.Equal(t, docsBefore, env.emb.documentCalls.Load(),
		"the backfill never embeds a title through the document path")
}

// TestEnsureTitleVectors_StopsAtTheLatencyBudget proves the budget is honoured
// and that partial coverage is REPORTED rather than hidden — a silently partial
// axis reads as "nothing to find".
func TestEnsureTitleVectors_StopsAtTheLatencyBudget(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 200)
	env.emb.perBatchDelay = 20 * time.Millisecond

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, 50*time.Millisecond)
	require.NoError(t, err)
	require.Greater(t, have, 0, "it makes progress")
	require.Less(t, have, total, "and stops before coverage completes")

	// Coverage completes over subsequent sessions rather than being abandoned.
	for range 20 {
		have, total, err = ensureTitleVectors(ctx, env.deps(), env.branch, 50*time.Millisecond)
		require.NoError(t, err)
		if have == total {
			break
		}
	}
	require.Equal(t, total, have, "later sessions finish the backfill")
}

// TestEnsureTitleVectors_NoEmbedderIsANoOp — read-only tooling and tests run
// without an embedder; the axis stays empty and nothing errors.
func TestEnsureTitleVectors_NoEmbedderIsANoOp(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnvWithoutEmbedder(t, 2)

	have, total, err := ensureTitleVectors(ctx, env.deps(), env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.Zero(t, have)
	require.Zero(t, total)
}

// TestRefreshShortlist_SeedsThenGoesIncremental is the performance contract:
// the first refresh covers the whole corpus, a second over an unchanged corpus
// does no work at all, and one edit costs one fact's worth of lookups — not the
// corpus's.
func TestRefreshShortlist_SeedsThenGoesIncremental(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 40)
	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)

	before := neighbourQueryCount.Load()
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))
	require.Equal(t, int64(40), neighbourQueryCount.Load()-before, "first refresh seeds every fact")

	stats, err := env.svc.Abstraction().RestatementPairStats(ctx, env.branch)
	require.NoError(t, err)
	require.Positive(t, stats.Count, "the cache is populated")

	before = neighbourQueryCount.Load()
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))
	require.Equal(t, before, neighbourQueryCount.Load(), "unchanged corpus does no work")

	env.writeFact("kb/f7.md", "F7 revised", "body-7-v2")
	_, _, err = ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)

	before = neighbourQueryCount.Load()
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))
	require.Equal(t, int64(1), neighbourQueryCount.Load()-before, "one edited fact, one lookup")
}

// TestRefreshShortlist_DropsPairsOfDepartedFacts — a stale pair naming a fact
// that no longer exists would be offered to the judge and fail to resolve.
func TestRefreshShortlist_DropsPairsOfDepartedFacts(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 6)
	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))

	_, err = env.svc.Facts().DeleteFact(ctx, env.branch, "kb/f3.md", "retract f3")
	require.NoError(t, err)
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))

	pairs, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 1000)
	require.NoError(t, err)
	for _, p := range pairs {
		require.NotEqual(t, "kb/f3.md", p.APath)
		require.NotEqual(t, "kb/f3.md", p.BPath)
	}
}

// TestRefreshShortlist_ExcludesPairsAtOrAboveDedup — the one absolute cosine in
// play is the model's OWN calibrated dedup threshold, and pairs at or above it
// are mergeFacts's business, not the judge's.
func TestRefreshShortlist_ExcludesPairsAtOrAboveDedup(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// Two facts with identical text: identical blended vectors, cosine 1.
	env.writeFact("kb/twin-a.md", "Twin", "identical body")
	env.writeFact("kb/twin-b.md", "Twin", "identical body")
	// ...and two whose titles are close while their bodies are not.
	env.writeFact("kb/near-a.md", "Near", "body one is about a thing")
	env.writeFact("kb/near-b.md", "Near two", "body two is about something else entirely")

	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))

	pairs, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 100)
	require.NoError(t, err)
	for _, p := range pairs {
		require.False(t, p.APath == "kb/twin-a.md" && p.BPath == "kb/twin-b.md",
			"a pair the mechanical dedup gate already catches must never reach the judge")
	}
	// ...while the sub-dedup pair still stands, so the assertion above is about
	// the filter rather than about the shortlist finding nothing.
	require.True(t, containsPair(pairs, "kb/near-a.md", "kb/near-b.md"),
		"a pair below the dedup floor is exactly what the judge should see")
}

// TestRefreshShortlist_KeepsSimilarToNeighbours is the roadmap's explicit trap.
// Genuine cross-cluster restatements are frequently top-K neighbours of each
// other, so "not SIMILAR_TO-adjacent" is NOT a substitute for the removed
// community condition — an adjacency exclusion would delete the target
// population. Here two facts are each other's nearest neighbours on both axes
// and must still be candidates.
func TestRefreshShortlist_KeepsSimilarToNeighbours(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/adjacent-a.md", "Adjacent restatement", "one body about the mechanism")
	env.writeFact("kb/adjacent-b.md", "Adjacent restatement", "another body about the same mechanism, worded differently and at greater length so the blended vectors diverge")

	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	require.NoError(t, refreshRestatementShortlist(ctx, d, env.branch, env.dedupThreshold()))

	pairs, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 100)
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	require.Equal(t, "kb/adjacent-a.md", pairs[0].APath)
	require.Equal(t, "kb/adjacent-b.md", pairs[0].BPath)
}
