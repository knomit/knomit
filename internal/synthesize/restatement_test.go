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

	refresh, err := refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)
	require.Equal(t, 40, refresh.NeighbourQueries, "first refresh seeds every fact")

	stats, err := env.svc.Abstraction().RestatementPairStats(ctx, env.branch)
	require.NoError(t, err)
	require.Positive(t, stats.Count, "the cache is populated")

	refresh, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)
	require.Zero(t, refresh.NeighbourQueries, "unchanged corpus does no work")

	env.writeFact("kb/f7.md", "F7 revised", "body-7-v2")
	_, _, err = ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)

	refresh, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)
	require.LessOrEqual(t, refresh.NeighbourQueries, 1+pairNeighbourK,
		"one edited fact costs its own lookup plus at most its partners'")
	require.Positive(t, refresh.NeighbourQueries)
}

// TestRefreshShortlist_DropsPairsOfDepartedFacts — a stale pair naming a fact
// that no longer exists would be offered to the judge and fail to resolve.
func TestRefreshShortlist_DropsPairsOfDepartedFacts(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 6)
	d := env.deps()
	_, _, err := ensureTitleVectors(ctx, d, env.branch, titleBackfillBudget)
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)

	_, err = env.svc.Facts().DeleteFact(ctx, env.branch, "kb/f3.md", "retract f3")
	require.NoError(t, err)
	_, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)

	pairs, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 1000)
	require.NoError(t, err)
	for _, p := range pairs {
		require.NotEqual(t, "kb/f3.md", p.APath)
		require.NotEqual(t, "kb/f3.md", p.BPath)
	}
}

// TestRefreshShortlist_KeepsPairsAtOrAboveDedup — the inverse of what this
// test asserted before #127.
//
// It used to pin "a pair the mechanical dedup gate already catches must never
// reach the judge". The premise was false: the mechanical gate only catches
// pairs inside ONE cluster, and this shortlist exists precisely for pairs whose
// halves cluster apart, so above-floor cross-cluster pairs were dropped here
// and caught nowhere. See the note where filterByBlendedCosine used to live.
//
// The real "prune already sees it" exclusion is asserted at its actual site, in
// restatement_certainty_test.go — a co-clustered pair still reaches the cache
// but is never enqueued.
func TestRefreshShortlist_KeepsPairsAtOrAboveDedup(t *testing.T) {
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
	_, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)

	pairs, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 100)
	require.NoError(t, err)
	require.True(t, containsPair(pairs, "kb/twin-a.md", "kb/twin-b.md"),
		"a certain duplicate is the shortlist's best candidate, not its waste — "+
			"no cluster-scoped merge reaches it when its halves cluster apart")
	// ...and the sub-floor pair still stands, so the assertion above is about
	// the removed filter rather than about the shortlist keeping everything
	// indiscriminately.
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
	_, err = refreshRestatementShortlist(ctx, d, env.branch)
	require.NoError(t, err)

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

// TestThrottleState_StartsOptimisticDefundsOnAllKeepRestoresOnResolution is the
// SOLE enforcement mechanism in phase 0: a corpus decides from its own judge's
// verdicts whether the shortlist is worth funding.
func TestThrottleState_StartsOptimisticDefundsOnAllKeepRestoresOnResolution(t *testing.T) {
	rate, state := throttleState(nil)
	require.Equal(t, throttleOptimistic, state, "no history means try it")
	require.Zero(t, rate)

	// Not enough evidence yet: a couple of keeps must not defund a corpus --
	// but they must not read as FUNDED either. This assertion said
	// throttleFunded because the code fell THROUGH to it, while the comment
	// above named the property actually under test. Judged-with-nothing-resolved
	// is its own state now (knomit#117b); both halves are asserted so the
	// not-defunded property cannot silently ride on the state name again.
	_, state = throttleState(keepVerdicts(throttleMinVerdicts - 1))
	require.Equal(t, throttleUnproven, state)
	require.NotEqual(t, throttleDefunded, state, "a couple of keeps must not defund a corpus")

	_, state = throttleState(keepVerdicts(throttleMinVerdicts))
	require.Equal(t, throttleDefunded, state, "enough judged, none resolved")

	withResolution := append(keepVerdicts(throttleMinVerdicts), store.RestatementVerdict{Resolved: true})
	rate, state = throttleState(withResolution)
	require.Equal(t, throttleFunded, state, "one resolution restores funding")
	require.Positive(t, rate)
}

// TestSelect_DefundedCorpusProbesAndRecovers is the test the earlier version of
// this suite got wrong. It used to "restore" funding by writing a resolution
// verdict directly — a state production can never reach, because a defunded
// corpus emits nothing, so it produces no verdicts, so its evidence can never
// change. Recovery has to come from a path the system can actually walk.
func TestSelect_DefundedCorpusProbesAndRecovers(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	// Decline enough pairs to defund the corpus.
	for range throttleMinVerdicts {
		pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
		require.NoError(t, err)
		require.NotEmpty(t, pairs)
		env.recordVerdict(pairs[0].APath, pairs[0].BPath, false)
	}
	pairs, health, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.Empty(t, pairs, "an all-keep history stops the spending")
	require.Equal(t, throttleDefunded, health.ThrottleState)

	// Quiet sessions while the probe interval elapses.
	for range throttleProbeInterval - 2 {
		pairs, health, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
		require.NoError(t, err)
		require.Empty(t, pairs)
		require.False(t, health.Probing)
	}

	// Then one probe slot, unprompted.
	pairs, health, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.Len(t, pairs, 1, "a defunded corpus spends exactly one slot to test its own verdict")
	require.True(t, health.Probing)
	require.Equal(t, throttleDefunded, health.ThrottleState)

	// The probe resolves — and the corpus funds itself again, from evidence it
	// could only have generated by probing.
	env.recordVerdict(pairs[0].APath, pairs[0].BPath, true)
	pairs, health, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.Equal(t, throttleFunded, health.ThrottleState)
	require.NotEmpty(t, pairs, "funding restored")
	require.False(t, health.Probing)
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

// TestSelect_DeclinedPairIsRetiredNotJustSkipped — a declined pair leaves the
// standing set entirely. Filtering it at selection time was not enough: the
// pair kept its place at the top of the ranking, so after a few declining
// sessions the whole selection window was pairs the judge had already refused
// and nothing new could be offered.
func TestSelect_DeclinedPairIsRetiredNotJustSkipped(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.NotEmpty(t, pairs)
	top := pairs[0]

	env.recordVerdict(top.APath, top.BPath, false)

	standing, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 10_000)
	require.NoError(t, err)
	require.False(t, containsPair(standing, top.APath, top.BPath),
		"the declined pair is gone from the standing set, not merely skipped")

	pairs, _, err = selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	require.NotEmpty(t, pairs, "and the slot goes to the next-ranked pair")
	require.False(t, containsPair(pairs, top.APath, top.BPath))
}

// TestRefresh_DeclinedPairIsNotReMintedByALaterRescan — retiring the pair is
// only half the job. A later neighbour rescan (triggered by an unrelated edit
// elsewhere) would happily rediscover it, so the verdict log gates minting too.
func TestRefresh_DeclinedPairIsNotReMintedByALaterRescan(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	top := pairs[0]
	env.recordVerdict(top.APath, top.BPath, false)

	// Force both endpoints back through KNN by evicting them from the cache
	// state — exactly what a partner requeue does.
	require.NoError(t, env.svc.Abstraction().ReplaceRestatementPairs(ctx, env.branch,
		[]int64{env.liveFactIDs()[top.APath], env.liveFactIDs()[top.BPath]}, nil, nil))
	env.seedShortlist()

	standing, err := env.svc.Abstraction().RestatementPairsByRank(ctx, env.branch, 10_000)
	require.NoError(t, err)
	require.False(t, containsPair(standing, top.APath, top.BPath),
		"a rescan must not resurrect a pair the judge has already refused")
}

// TestSelect_EditingEitherFactReEligibilizesTheDeclinedPair — the guard is
// keyed by fact id, and ids are content-addressed, so a decline expires
// structurally when either fact changes rather than by any staleness rule.
func TestSelect_EditingEitherFactReEligibilizesTheDeclinedPair(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 400)
	d := env.deps()
	env.seedShortlist()

	pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, 400)
	require.NoError(t, err)
	top := pairs[0]
	env.recordVerdict(top.APath, top.BPath, false)

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

// TestApply_AttributesOnlyShortlistVerdicts — the throttle is only as good as
// its attribution. If an ordinary cluster prune counted, a healthy corpus's
// cluster merges would keep a useless shortlist funded forever.
func TestApply_AttributesOnlyShortlistVerdicts(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/pair-a.md", "Shared headline", "one account")
	env.writeFact("kb/pair-b.md", "Shared headline", "a different account of the same thing, longer")
	for i := range 400 {
		env.writeFact(fmt.Sprintf("kb/filler%d.md", i), fmt.Sprintf("Filler %d", i), fmt.Sprintf("filler body %d", i))
	}

	d := env.deps()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)
	env.seedShortlist()

	// An ordinary cluster item, answered with a merge: must NOT be attributed.
	clusterItem := &store.PipelineWorkItem{
		SessionID: sess.ID, StepType: "prune", ClusterKey: "cluster-0",
		FactsJSON: env.factsJSON("kb/filler0.md", "kb/filler1.md"),
	}
	judged := resolveShortlistPair(ctx, d, sess, clusterItem)
	require.Nil(t, judged, "a cluster item is not a shortlist outcome")
	recordShortlistVerdict(ctx, d, sess, judged, &PruneResult{
		Merges: []MergeEntry{{Paths: []string{"kb/filler0.md", "kb/filler1.md"}}},
	})

	verdicts, err := env.svc.Abstraction().RecentRestatementVerdicts(ctx, env.branch, throttleWindow)
	require.NoError(t, err)
	require.Empty(t, verdicts)

	// A shortlist item, answered with keep: attributed, and not merged.
	restateItem := &store.PipelineWorkItem{
		SessionID: sess.ID, StepType: "prune", ClusterKey: restatementClusterKeyPrefix + "0",
		FactsJSON: env.factsJSON("kb/pair-a.md", "kb/pair-b.md"),
	}
	judged = resolveShortlistPair(ctx, d, sess, restateItem)
	require.NotNil(t, judged)
	require.NotZero(t, judged.AFactID, "the versions the judge saw are captured by id")
	recordShortlistVerdict(ctx, d, sess, judged, &PruneResult{})

	verdicts, err = env.svc.Abstraction().RecentRestatementVerdicts(ctx, env.branch, throttleWindow)
	require.NoError(t, err)
	require.Len(t, verdicts, 1)
	require.False(t, verdicts[0].Resolved)

	// The same item answered with a merge OF THAT PAIR is attributed as resolved.
	recordShortlistVerdict(ctx, d, sess, judged, &PruneResult{
		Merges: []MergeEntry{{Paths: []string{"kb/pair-a.md", "kb/pair-b.md"}}},
	})
	verdicts, err = env.svc.Abstraction().RecentRestatementVerdicts(ctx, env.branch, throttleWindow)
	require.NoError(t, err)
	require.Len(t, verdicts, 2)
	require.True(t, verdicts[0].Resolved, "newest first")

	// ...and so is a RETRACT of either half. A judge that consolidates by
	// retracting the redundant fact has done the work this mechanism buys;
	// counting only merges would defund a corpus that is succeeding.
	recordShortlistVerdict(ctx, d, sess, judged, &PruneResult{
		Decisions: []PruneDecision{{Path: "kb/pair-b.md", Action: "retract"}},
	})
	verdicts, err = env.svc.Abstraction().RecentRestatementVerdicts(ctx, env.branch, throttleWindow)
	require.NoError(t, err)
	require.True(t, verdicts[0].Resolved, "a retract of one half resolves the pair")

	// A confidence update does NOT: both facts still stand, so the redundancy
	// the pair was offered for is still there.
	recordShortlistVerdict(ctx, d, sess, judged, &PruneResult{
		Decisions: []PruneDecision{{Path: "kb/pair-a.md", Action: "update", Confidence: 0.9}},
	})
	verdicts, err = env.svc.Abstraction().RecentRestatementVerdicts(ctx, env.branch, throttleWindow)
	require.NoError(t, err)
	require.False(t, verdicts[0].Resolved, "an update leaves the redundancy in place")
}

// TestApply_SkipsVerdictWhenAnEndpointIsAlreadyGone — a pair whose halves were
// merged or retracted earlier in the same session is judging something that no
// longer exists. Recording it would write fact id 0 into the declined set,
// where it matches every other unresolved pair, and would spend a slot of the
// throttle window on a non-event.
func TestApply_SkipsVerdictWhenAnEndpointIsAlreadyGone(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 4)
	d := env.deps()
	sess, err := env.svc.Pipeline().CreatePipelineSession(ctx, "review", env.branch, "")
	require.NoError(t, err)

	item := &store.PipelineWorkItem{
		SessionID: sess.ID, StepType: "prune", ClusterKey: restatementClusterKeyPrefix + "0",
		FactsJSON: env.factsJSON("kb/f0.md", "kb/f1.md"),
	}
	judged := resolveShortlistPair(ctx, d, sess, item)
	require.NotNil(t, judged)

	// f1 is retracted before the verdict is recorded — the earlier-item case.
	_, err = env.svc.Facts().DeleteFact(ctx, env.branch, "kb/f1.md", "retract f1")
	require.NoError(t, err)
	gone := resolveShortlistPair(ctx, d, sess, item)
	require.NotNil(t, gone)
	require.Zero(t, gone.BFactID, "the retracted endpoint no longer resolves")

	recordShortlistVerdict(ctx, d, sess, gone, &PruneResult{})
	verdicts, err := env.svc.Abstraction().RecentRestatementVerdicts(ctx, env.branch, throttleWindow)
	require.NoError(t, err)
	require.Empty(t, verdicts, "no verdict is recorded for a pair that no longer exists")
}
