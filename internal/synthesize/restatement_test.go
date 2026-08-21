package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
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

// TestShortlistBudget_ScalesWithCorpusAndCapsOut — the cap is also the
// cold-start guard: a brand-new corpus risks a slot or two before any mechanism
// here has learned anything about it.
func TestShortlistBudget_ScalesWithCorpusAndCapsOut(t *testing.T) {
	require.Equal(t, 0, shortlistBudget(0))
	require.Equal(t, 0, shortlistBudget(150), "a tiny corpus spends nothing")
	require.Equal(t, 1, shortlistBudget(300))
	require.Equal(t, 4, shortlistBudget(800))
	require.Equal(t, maxShortlistItems, shortlistBudget(50_000), "capped however large the corpus")
}

// TestThrottleState_StartsOptimisticDefundsOnAllKeepRestoresOnMerge is the
// SOLE enforcement mechanism in phase 0: a corpus decides from its own judge's
// verdicts whether the shortlist is worth funding.
func TestThrottleState_StartsOptimisticDefundsOnAllKeepRestoresOnMerge(t *testing.T) {
	rate, state := throttleState(nil)
	require.Equal(t, throttleOptimistic, state, "no history means try it")
	require.Zero(t, rate)

	// Not enough evidence yet: a couple of keeps must not defund a corpus.
	_, state = throttleState(keepVerdicts(throttleMinVerdicts - 1))
	require.Equal(t, throttleFunded, state)

	_, state = throttleState(keepVerdicts(throttleMinVerdicts))
	require.Equal(t, throttleDefunded, state, "enough judged, none merged")

	withMerge := append(keepVerdicts(throttleMinVerdicts), store.RestatementVerdict{Merged: true})
	rate, state = throttleState(withMerge)
	require.Equal(t, throttleFunded, state, "one merge restores funding")
	require.Positive(t, rate)
}

// TestSelect_ThrottleDefundsAndAMergeRestores drives the throttle through the
// real store rather than the pure function above.
func TestSelect_ThrottleDefundsAndAMergeRestores(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, health, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.NotEmpty(t, pairs, "a fresh corpus is funded")
	require.Equal(t, throttleOptimistic, health.ThrottleState)
	require.Equal(t, len(pairs), health.Emitted)
	require.Positive(t, health.OperatingPoint, "the cut is reported as a cosine")

	for i := range throttleMinVerdicts {
		env.recordVerdict(fmt.Sprintf("kb/f%d.md", i), "kb/f399.md", false)
	}
	pairs, health, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.Empty(t, pairs, "all-keep history zeroes the corpus's shortlist budget")
	require.Equal(t, throttleDefunded, health.ThrottleState)
	require.Positive(t, health.StandingPairs, "the pairs still stand; the corpus just stops paying to judge them")

	env.recordVerdict("kb/f1.md", "kb/f2.md", true)
	pairs, health, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.NotEmpty(t, pairs, "a merge restores funding")
	require.Equal(t, throttleFunded, health.ThrottleState)
}

// TestSelect_ExcludesPairsCoGroupedByThisSessionsClusters — the exact "prune
// already sees them together" exclusion, done as a membership check over
// clusters the caller already computed.
func TestSelect_ExcludesPairsCoGroupedByThisSessionsClusters(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.NotEmpty(t, pairs)
	top := pairs[0]

	// Put the top pair in one cluster and it stops being worth a slot.
	clusters := [][]factForLLM{{{File: top.APath}, {File: top.BPath}}}
	pairs, _, err = selectRestatementCandidates(ctx, d, env.branch, clusters, 400)
	require.NoError(t, err)
	require.False(t, containsPair(pairs, top.APath, top.BPath),
		"a pair prune already sees in one cluster must not also cost a shortlist slot")
}

// TestSelect_KeptPairIsNotReOffered — the throttle defunds a CORPUS; this is
// what stops one declined pair from occupying the funded slots forever.
func TestSelect_KeptPairIsNotReOffered(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.NotEmpty(t, pairs)
	top := pairs[0]

	env.recordVerdict(top.APath, top.BPath, false)

	pairs, _, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.False(t, containsPair(pairs, top.APath, top.BPath),
		"the judge already looked at this pair and said keep")
	require.NotEmpty(t, pairs, "and the slot goes to the next-ranked pair instead")
}

// TestSelect_EditingEitherFactReEligibilizesTheKeptPair — the exclusion is
// keyed by fact id, and ids are content-addressed, so a keep expires
// structurally when either fact changes rather than by any staleness rule.
func TestSelect_EditingEitherFactReEligibilizesTheKeptPair(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	top := pairs[0]
	env.recordVerdict(top.APath, top.BPath, false)

	pairs, _, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.False(t, containsPair(pairs, top.APath, top.BPath))

	// Rewrite one endpoint with the same title (so it stays a candidate pair)
	// but a different body: new blob, new facts row, new id.
	env.writeFact(top.APath, env.titleOf(top.APath), "a materially rewritten body for the same claim")
	env.seedShortlist()

	pairs, _, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.True(t, containsPair(pairs, top.APath, top.BPath),
		"an edited fact is a new version, and the judge has not seen this one")
}

// TestSelect_ScopedSessionEmitsOnlyPairsTouchingScope — the standing shortlist
// stays corpus-wide (that is the whole point), but a session asked to work on
// one area does not spend its slots on two facts that are both elsewhere.
func TestSelect_ScopedSessionEmitsOnlyPairsTouchingScope(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFactWithDomain("kb/auth-a.md", "Token refresh", "one body", "auth")
	env.writeFactWithDomain("kb/auth-b.md", "Token refresh", "another body entirely, worded differently", "auth")
	env.writeFactWithDomain("kb/store-a.md", "Row locking", "one body", "store")
	env.writeFactWithDomain("kb/store-b.md", "Row locking", "another body entirely, worded differently", "store")

	d := env.deps()
	env.seedShortlist()

	// Corpus-wide: both pairs stand.
	all, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 100)
	require.NoError(t, err)
	require.True(t, containsPair(all, "kb/auth-a.md", "kb/auth-b.md"))
	require.True(t, containsPair(all, "kb/store-a.md", "kb/store-b.md"))

	// Scoped emission: only the pair touching the scope is offered.
	scoped := d
	scoped.Scope = ScopeFilter{Domain: []string{"auth"}}
	pairs, _, err := selectRestatementCandidates(ctx, scoped, env.branch, nil, 4000)
	require.NoError(t, err)
	require.True(t, containsPair(pairs, "kb/auth-a.md", "kb/auth-b.md"))
	require.False(t, containsPair(pairs, "kb/store-a.md", "kb/store-b.md"),
		"a scoped session does not spend its slots outside its scope")
}

// TestSelect_OperatingPointIsPercentileDerivedPerCorpus — same code, two
// corpora, two different absolute cosines. This is what MN13 buys: nothing here
// encodes a claim about what a restatement looks like.
func TestSelect_OperatingPointIsPercentileDerivedPerCorpus(t *testing.T) {
	ctx := context.Background()

	// Two corpora of the same size whose TITLES are shaped differently: in one
	// they crowd together, in the other they stand apart. The seed count passed
	// to the selector is the same for both, so any difference in the reported
	// cut comes from the corpora, not from the budget.
	tight := newRestatementEnvOnAxis(t, 20, 0.01)
	tight.seedShortlist()
	_, tightHealth, err := selectRestatementCandidates(ctx, tight.deps(), tight.branch, nil, 400)
	require.NoError(t, err)

	loose := newRestatementEnvOnAxis(t, 20, 0.25)
	loose.seedShortlist()
	_, looseHealth, err := selectRestatementCandidates(ctx, loose.deps(), loose.branch, nil, 400)
	require.NoError(t, err)

	require.Positive(t, tightHealth.OperatingPoint)
	require.Positive(t, looseHealth.OperatingPoint)
	require.Greater(t, tightHealth.OperatingPoint, looseHealth.OperatingPoint+0.01,
		"the cut is a percentile of each repo's own distribution, not a fixed cosine")
}

// TestPlan_EmitsRestatementItemsAsOrdinaryPruneItems — shape parity with
// cluster items is what lets the existing prompt, schema, and apply path serve
// these unchanged.
func TestPlan_EmitsRestatementItemsAsOrdinaryPruneItems(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// Two facts with near-identical titles and unrelated bodies, plus filler so
	// the corpus is large enough to fund a slot.
	env.writeFact("kb/restate-a.md", "Cache invalidation on write", "one account of it")
	env.writeFact("kb/restate-b.md", "Cache invalidation on write", "a different account, at length, of the same thing")
	for i := range 400 {
		env.writeFact(fmt.Sprintf("kb/filler%d.md", i), fmt.Sprintf("Filler %d", i), fmt.Sprintf("filler body %d", i))
	}

	res, err := NewReviewerWithOptions(env.ri, nil, EffortNormal, ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, res.SessionID)

	items := env.workItems(res.SessionID)
	var restatement []store.PipelineWorkItem
	for _, it := range items {
		if strings.HasPrefix(it.ClusterKey, restatementClusterKeyPrefix) {
			restatement = append(restatement, it)
		}
	}
	require.NotEmpty(t, restatement, "the shortlist put something in front of the judge")
	for _, it := range restatement {
		require.Equal(t, "prune", it.StepType, "a restatement item IS a prune item")
		var facts []factForLLM
		require.NoError(t, json.Unmarshal([]byte(it.FactsJSON), &facts))
		require.Len(t, facts, 2, "exactly the pair")
		require.NotEmpty(t, facts[0].Body, "with the bodies the judge needs")
	}
}

// TestPlan_HealthLinesReachTheCaller — a partial axis or a defunded shortlist
// has to be visible, or "no candidates" reads as "nothing to find".
func TestPlan_HealthLinesReachTheCaller(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 30)

	res, err := NewReviewerWithOptions(env.ri, nil, EffortNormal, ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)

	joined := strings.Join(res.Health, "\n")
	require.Contains(t, joined, "abstraction coverage")
	require.Contains(t, joined, "standing restatement pairs")
	require.Contains(t, joined, "operating point")
	require.Contains(t, joined, "restatement candidates emitted")
	require.Contains(t, joined, "shortlist throttle")
}

// TestPlan_FloodCorpusIsBoundedByTheBudgetNotSuppressed — the design has no
// density cut: a corpus where title similarity does not discriminate still gets
// a cap-sized batch, and it is that corpus's OWN verdicts that defund it.
func TestPlan_FloodCorpusIsBoundedByTheBudgetNotSuppressed(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	// Every title identical: on this corpus the axis says everything restates
	// everything.
	for i := range 600 {
		env.writeFact(fmt.Sprintf("kb/flood%d.md", i), "Same headline every time",
			fmt.Sprintf("body %d, quite different in wording from the others", i))
	}

	res, err := NewReviewerWithOptions(env.ri, nil, EffortNormal, ScopeFilter{}).StartSession(ctx)
	require.NoError(t, err)

	emitted := 0
	for _, it := range env.workItems(res.SessionID) {
		if strings.HasPrefix(it.ClusterKey, restatementClusterKeyPrefix) {
			emitted++
		}
	}
	require.LessOrEqual(t, emitted, maxShortlistItems,
		"bounded by the judge-slot budget, never by a rate anyone hard-coded")
	require.Contains(t, strings.Join(res.Health, "\n"), "standing restatement pairs",
		"and the flood is visible in the health output")
}
