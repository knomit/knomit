package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// The structural allowance (knomit#155).
//
// The route existed and was inert. Detection minted thousands of path-identity
// pairs and offered zero, on every corpus that most needed them, because the
// route's cost was expressed in the ordinary band's currency: it sat behind
// `budget >= 2`, and a defunded corpus probes with budget 1. The corpus whose
// judge had resolved nothing could never be shown the evidence that might
// change its mind — the penalty blocking its own recovery.
//
// These tests pin the four properties that break the latch. Each is written to
// fail for one reason only; the sabotage that catches each is named in the
// commit.

// structuralDefundedEnv is a corpus with one path-identity pair, driven to the
// DEFUNDED throttle state by an all-keep verdict history.
//
// Defunding is done through the real verdict path rather than by poking the
// throttle, because the latch is a composition of two mechanisms and a test
// that fakes one of them cannot see it.
func structuralDefundedEnv(t *testing.T, a, b string) (*restatementEnv, Deps) {
	t.Helper()
	ctx := context.Background()
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload"),
	})
	env.seedShortlist()
	d := env.deps()

	// The fixture MUST reach the defunded state, and MUST still hold the
	// structural pair when it gets there. Both are asserted below rather than
	// assumed: a fixture chosen to make a test discriminate is a claim, and an
	// unasserted claim is how a test ends up comparing zero against zero.
	for range throttleMinVerdicts {
		pairs, _, err := selectRestatementCandidates(ctx, d, env.branch, nil, structuralFiller+2)
		require.NoError(t, err)
		require.NotEmpty(t, pairs, "the corpus must still be spending while it is being defunded")
		// Decline an ORDINARY pair. Declining the structural pair would retire
		// the very thing under test.
		var declined bool
		for _, p := range pairs {
			if p.MatchKind == store.MatchTitleKNN {
				env.recordVerdict(p.APath, p.BPath, false)
				declined = true
				break
			}
		}
		require.True(t, declined, "fixture must offer an ordinary pair to decline")
	}
	return env, d
}

// TestStructural_AllowanceFiresOnADefundedCorpus is the latch break, and it is
// the whole of knomit#155.
//
// A defunded corpus that is NOT probing used to return nothing at all — the
// early return happened before the structural fetch was even reached, so the
// route was unreachable in exactly the state it was built for. The throttle
// governs spend on TITLE-RANKED candidates, which is what its verdict history
// is evidence about; it has never judged a structural pair, so it has no
// evidence on which to withhold one.
func TestStructural_AllowanceFiresOnADefundedCorpus(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env, d := structuralDefundedEnv(t, a, b)

	pairs, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, structuralFiller+2)
	require.NoError(t, err)

	// Fixture assertions FIRST — without these, every assertion below could
	// pass against an empty population.
	require.Equal(t, throttleDefunded, h.ThrottleState,
		"fixture must actually be defunded, or this test measures the funded path")
	require.False(t, h.Probing,
		"fixture must be in the SILENT defunded state, not on a probe session — "+
			"the probe path was never the one that was latched shut")
	require.Positive(t, h.StandingStructural,
		"fixture must hold standing structural pairs, or 'offered' is compared against nothing")

	require.Positive(t, h.StructuralOffered,
		"a defunded corpus must still be shown structural evidence — its verdict "+
			"history is about title-ranked pairs and says nothing about this route")
	require.True(t, containsPair(pairs, a, b),
		"the structurally matched pair is what must reach the judge, not merely a count")
}

// TestStructural_AllowanceIsBoundedOnADefundedCorpus is the other half of the
// latch break, and the reason the allowance is a number rather than a flag.
//
// "Fires even when defunded" without a bound is not a fix, it is a corpus that
// ignores its own throttle. The allowance is a RESOURCE BUDGET (MN13): the
// route is reachable, and it is bounded, and the two are independent claims.
func TestStructural_AllowanceIsBoundedOnADefundedCorpus(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env, d := structuralDefundedEnv(t, a, b)

	pairs, h, err := selectRestatementCandidates(ctx, d, env.branch, nil, structuralFiller+2)
	require.NoError(t, err)
	require.Equal(t, throttleDefunded, h.ThrottleState)
	// Fixture assertion, and it is not ceremony: `0 <= structuralAllowance` is
	// true, so without this the bound below is satisfied by a route that offers
	// NOTHING — which is precisely the bug this whole commit removes. Caught by
	// sabotage S1, which nulled the sweep on the throttle-closed path and left
	// this test green.
	require.Positive(t, h.StructuralOffered,
		"fixture must actually offer structural pairs, or the bound is asserted against zero")
	require.LessOrEqual(t, len(pairs), structuralAllowance,
		"a defunded corpus spends the allowance and NOTHING else — the ordinary "+
			"band stays shut, which is what the throttle actually decided")
	require.Equal(t, h.StructuralOffered, len(pairs),
		"every pair a defunded corpus emits comes from the allowance")
}

// TestStructural_SweepIsOldestFirstNotHighestCosine pins the sweep order.
//
// Inflow far exceeds what any session can judge, so re-selecting the top of a
// cosine ranking every session starves the tail forever — and the ranking buys
// nothing on this route anyway, since a structurally matched pair is a
// near-certain duplicate whatever its cosine happens to be.
//
// The test is written as a DISAGREEMENT between the two orderings: it asserts
// the sweep returns something the cosine ranking does not put first. An
// assertion that merely checked "the sweep returned pairs" would pass under a
// sabotage that swapped the ORDER BY back to `title_cos DESC`.
func TestStructural_SweepIsOldestFirstNotHighestCosine(t *testing.T) {
	ctx := context.Background()
	env := sweepOrderEnv(t)
	env.seedShortlist()
	ax := env.svc.Abstraction()

	byCos, err := ax.RestatementPairsByMatchKind(ctx, env.branch,
		[]string{store.MatchPathIdentity}, 100_000)
	require.NoError(t, err)
	byAge, err := ax.RestatementPairsByMatchKindOldest(ctx, env.branch,
		[]string{store.MatchPathIdentity}, 100_000)
	require.NoError(t, err)

	// Fixture assertion: the two orderings can only DISAGREE if the population
	// is big enough to have an order at all, and if cosine is not already
	// monotone in mint order.
	require.Equal(t, len(byCos), len(byAge),
		"both readers must see the same population; only the order may differ")
	require.Greater(t, len(byAge), 1,
		"fixture must hold more than one structural pair, or 'order' is meaningless")
	require.NotEqual(t, orderOf(byCos), orderOf(byAge),
		"fixture must be one where mint order and cosine rank actually disagree, "+
			"or this test cannot tell the two readers apart")

	// And the sweep is NOT sorted by descending cosine — that is the property
	// under test, stated directly.
	require.False(t, isDescendingByCos(byAge),
		"the sweep must drain in mint order; a cosine-ranked sweep re-offers the "+
			"same head every session and never reaches the tail")
}

// TestStructural_RareTokenStaysClosed pins the sampling stage.
//
// Path-identity's merge rate is UNMEASURED — the route it shares was inert, so
// zero structural pairs have ever been judged. Offering both routes at once
// would spend that sample on a mixed population whose halves cannot be told
// apart afterwards, which is the one thing the staging exists to prevent.
func TestStructural_RareTokenStaysClosed(t *testing.T) {
	ctx := context.Background()
	// A shared rare identifier, and NOT a path identity: neither path's token
	// set contains the other's (`hardware` vs `policy`), so the only evidence
	// joining them is the CVE in both titles.
	a := "kb/technology/hardware/storage/aaaaaab1.md"
	b := "kb/technology/policy/procurement/aaaaaab2.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "CVE-2026-16232 forces an emergency storage firmware recall"),
		namedFact(b, "Procurement halts after CVE-2026-16232 lands in the catalogue"),
	})
	env.seedShortlist()

	// Fixture assertion: the pair must actually have been DETECTED as
	// rare-token, or "not offered" says nothing about the gate under test.
	require.Equal(t, store.MatchRareToken, pairKindIn(t, env, a, b),
		"fixture must produce a rare-token match, or the gate is not being tested")

	sweep, err := selectStructuralSweep(ctx, env.deps(), env.branch, map[string]struct{}{})
	require.NoError(t, err)
	require.False(t, containsPair(sweep, a, b),
		"rare-token is the weaker, wider net and stays closed until the "+
			"path-identity sample says what a structural offer is worth")
	for _, p := range sweep {
		require.Equal(t, store.MatchPathIdentity, p.MatchKind,
			"the sweep offers path-identity only while the sample is running")
	}
}

// TestStructural_CoClusteredPairIsStillExcluded pins the ONE exclusion that
// survives on this route.
//
// Dropping the scope filter is deliberate; dropping this would not be. "Prune
// already sees this pair in one cluster" means a shortlist slot buys a second
// judgement of the same question — true whatever route found the pair.
func TestStructural_CoClusteredPairIsStillExcluded(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload"),
	})
	env.seedShortlist()
	d := env.deps()

	// Fixture assertion: unclustered, the pair IS offered. Without this the
	// test below passes against a route that offers nothing at all.
	free, err := selectStructuralSweep(ctx, d, env.branch, map[string]struct{}{})
	require.NoError(t, err)
	require.True(t, containsPair(free, a, b),
		"fixture must offer the pair when it is NOT co-clustered, or the "+
			"exclusion is being credited for a route that was already empty")

	coGrouped := clusterCoMembership([][]factForLLM{{{File: a}, {File: b}}})
	held, err := selectStructuralSweep(ctx, d, env.branch, coGrouped)
	require.NoError(t, err)
	require.False(t, containsPair(held, a, b),
		"a pair prune already sees in one cluster must not also cost a shortlist slot")
}

// TestStructural_ScopedSessionIsOfferedOutOfScopePairsAndToldSo is the
// scope-widening half, and BOTH assertions are load-bearing.
//
// The designer ruled the structural route ignores the scope filter: a
// structural pair's evidence is the corpus's own filing, which is corpus-wide
// by construction, and no scoped view would ever co-present the two halves. A
// scope that silently widens is nonetheless the #122 family's own defect — so
// the widening is announced. Testing the widening without the announcement
// would pin exactly half of what was ruled.
func TestStructural_ScopedSessionIsOfferedOutOfScopePairsAndToldSo(t *testing.T) {
	ctx := context.Background()
	a := "kb/technology/security/vulnerabilities/cisco/1e8287a2.md"
	b := "kb/technology/security/vulnerabilities/networking/cisco/bfbb31b8.md"
	env := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact(a, "Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact(b, "Nine Cisco advisories cover Crosswork and Secure Workload"),
	})
	env.seedShortlist()

	scoped := env.deps()
	// A domain no fact in this corpus carries, so EVERY structural pair the
	// sweep returns is out of scope by construction.
	scoped.Scope = ScopeFilter{Domain: []string{"a-domain-no-fact-here-carries"}}

	pairs, h, err := selectRestatementCandidates(ctx, scoped, env.branch, nil, structuralFiller+2)
	require.NoError(t, err)

	require.Positive(t, h.StructuralOffered,
		"the structural route does not answer to the scope filter — a cross-category "+
			"structural pair is corpus-wide evidence by construction")
	require.True(t, containsPair(pairs, a, b))
	require.Equal(t, h.StructuralOffered, h.StructuralOutOfScope,
		"every pair offered here is outside the scope, and the count must say so")
	require.Contains(t, structuralSignalLine(h), "OUTSIDE this session's scope",
		"a scope that widens without saying so is the defect #122 exists to end")
}

// TestStructural_HealthNamesTheSweepRegime — "oldest-first" is exact only once
// the title axis is complete. While it fills, every pair is re-minted each
// session and mint order churns, so the sweep is deterministic but not
// age-stable. A degenerated sweep order reads identically to a working one from
// outside, which is why the regime is stated rather than implied.
func TestStructural_HealthNamesTheSweepRegime(t *testing.T) {
	full := restatementHealth{StandingStructural: 3, Coverage: 1, SweepOrderStable: true}
	require.Contains(t, structuralSignalLine(full), "oldest-first (axis complete)")

	filling := restatementHealth{StandingStructural: 3, Coverage: 0.4, SweepOrderStable: false}
	require.Contains(t, structuralSignalLine(filling), "NOT yet age-stable")
	require.NotContains(t, structuralSignalLine(filling), "oldest-first (axis complete)",
		"a filling axis must not be described as if its sweep order were age-stable")
}

// TestStructural_SweepRegimeIsDerivedNotDeclared is the other half of the
// regime report, and it exists because the test above is not enough.
//
// The one above hands `structuralSignalLine` a health struct built by hand, so
// it pins the RENDERING and nothing else — a sabotage that hardcodes
// `SweepOrderStable = true` at the point of derivation walks straight through
// it (it did: sabotage S7). Asserting on a value a test supplied is asserting
// that the formatter works. This drives the real derivation instead, which is
// the campaign's path-vs-state rule applied to a health line.
func TestStructural_SweepRegimeIsDerivedNotDeclared(t *testing.T) {
	ctx := context.Background()

	// A corpus with NO embedder never puts a fact on the title axis, so
	// coverage is zero and the sweep order cannot be age-stable.
	bare := newRestatementEnvWithoutEmbedder(t, 12)
	_, hBare, err := selectRestatementCandidates(ctx, bare.deps(), bare.branch, nil, 12)
	require.NoError(t, err)
	require.Zero(t, hBare.Coverage, "fixture must have an EMPTY axis, or it is not the filling regime")
	require.False(t, hBare.SweepOrderStable,
		"an axis with no coverage cannot have an age-stable sweep order — every "+
			"pair is re-minted the moment the axis starts filling")

	// A corpus whose axis is fully backfilled reports the stable regime.
	full := structuralEnv(t, [2]struct{ Path, Title string }{
		namedFact("kb/technology/security/vulnerabilities/cisco/aaaaaa07.md",
			"Cisco patches nine flaws across Crosswork and Secure Workload"),
		namedFact("kb/technology/security/vulnerabilities/networking/cisco/aaaaaa08.md",
			"Nine Cisco advisories cover Crosswork and Secure Workload"),
	})
	full.seedShortlist()
	_, hFull, err := selectRestatementCandidates(ctx, full.deps(), full.branch, nil, structuralFiller+2)
	require.NoError(t, err)
	require.EqualValues(t, 1, hFull.Coverage,
		"fixture must have a COMPLETE axis, or it is not the stable regime")
	require.True(t, hFull.SweepOrderStable,
		"a complete axis is exactly when mint order stops churning and oldest-first "+
			"means what it says")
}

// TestStructural_AllowanceCapsTheSweep pins the BOUND, on a corpus that can
// actually exceed it.
//
// This test exists because the bound was unguarded and nothing noticed: every
// other fixture here holds fewer standing path-identity pairs than the
// allowance, so deleting the cap left them all green (sabotage S8). A cap can
// only be tested by a population large enough to hit it — otherwise the
// over-fetch limit does the capping and the test credits the wrong line.
func TestStructural_AllowanceCapsTheSweep(t *testing.T) {
	ctx := context.Background()
	const pairs = structuralAllowance + 3
	env := manyTwinsEnv(t, pairs)
	env.seedShortlist()
	d := env.deps()

	// Fixture assertion: the corpus must hold MORE structural pairs than the
	// allowance, or the cap is never reached and this measures nothing.
	standing, err := env.svc.Abstraction().RestatementPairsByMatchKindOldest(ctx, env.branch,
		[]string{store.MatchPathIdentity}, 100_000)
	require.NoError(t, err)
	require.Greater(t, len(standing), structuralAllowance,
		"fixture must hold more standing path-identity pairs than the allowance, "+
			"or the cap under test is never reached")

	sweep, err := selectStructuralSweep(ctx, d, env.branch, map[string]struct{}{})
	require.NoError(t, err)
	require.Len(t, sweep, structuralAllowance,
		"the sweep spends the allowance and stops — an unbounded sweep would hand "+
			"a whole session's judge budget to one route")
}

// manyTwinsEnv builds a corpus with n path-identity twin pairs, each on its own
// discriminating subject token so no pair can match across pairs.
func manyTwinsEnv(t *testing.T, n int) *restatementEnv {
	t.Helper()
	// Subject tokens chosen to be rare in this corpus and unrelated to each
	// other; the prefix-EXTENSION shape (subject vs vendor/subject) is the one
	// measured on the live corpus.
	subjects := []string{
		"cisco", "gitlab", "geoserver", "sharepoint", "ivanti",
		"sonicwall", "fortinet", "confluence", "jenkins", "grafana",
	}
	require.LessOrEqual(t, n, len(subjects), "manyTwinsEnv has only %d subjects", len(subjects))

	env := newRestatementEnv(t, 0)
	for i := range structuralFiller {
		env.writeFact(
			fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1),
			fmt.Sprintf("Filler note 2026 about widget %d", 1000+i),
			"an unrelated body")
	}
	for i, s := range subjects[:n] {
		env.writeFact(
			fmt.Sprintf("kb/technology/security/vulnerabilities/%s/%08x.md", s, 0xAA0000+i*2),
			fmt.Sprintf("%s advisory covers a remote code execution flaw", s),
			"a body about the event")
		env.writeFact(
			fmt.Sprintf("kb/technology/security/vulnerabilities/vendor/%s/%08x.md", s, 0xAA0001+i*2),
			fmt.Sprintf("Remote code execution flaw disclosed in %s", s),
			"the same event, filed again")
	}
	return env
}

// sweepOrderEnv builds a corpus where MINT ORDER and COSINE RANK are exact
// reverses of each other, so the two readers cannot agree by accident.
//
// Three path-identity twin pairs are written in sequence — mint order is write
// order, since the refresh rescans in fact-id order and fact ids are assigned
// at write time — and their title cosines INCREASE down that sequence. So the
// cosine ranking reads P3, P2, P1 and the sweep reads P1, P2, P3.
//
// The twins are placed by hand on the axis rather than left to the hash
// embedder: with hashed vectors the two orderings would probably differ, and
// "probably" is not a fixture. Each pair sits at its own base angle, far from
// the others, so no cross-pair title-KNN pair can crowd the population under
// test; and each carries its own discriminating path token, so no cross-pair
// PATH-IDENTITY match can form either.
func sweepOrderEnv(t *testing.T) *restatementEnv {
	t.Helper()
	type twin struct {
		aPath, bPath   string
		aTitle, bTitle string
		base, delta    float64
	}
	// delta DECREASES down the list, so cos(delta) — the pair's title cosine —
	// increases: 0.83, 0.92, 0.98. Mint order is therefore ascending cosine,
	// the exact reverse of the ranking.
	twins := []twin{
		{
			aPath:  "kb/technology/security/vulnerabilities/cisco/aaaaaa01.md",
			bPath:  "kb/technology/security/vulnerabilities/networking/cisco/aaaaaa02.md",
			aTitle: "Cisco patches nine flaws across Crosswork and Secure Workload",
			bTitle: "Nine Cisco advisories cover Crosswork and Secure Workload",
			base:   0.0, delta: 0.6,
		},
		{
			aPath:  "kb/technology/security/vulnerabilities/gitlab/aaaaaa03.md",
			bPath:  "kb/technology/security/vulnerabilities/devops/gitlab/aaaaaa04.md",
			aTitle: "GitLab directive flaw exploited within days of disclosure",
			bTitle: "Attackers reach GitLab through a directive flaw",
			base:   2.0, delta: 0.4,
		},
		{
			aPath:  "kb/technology/security/vulnerabilities/geoserver/aaaaaa05.md",
			bPath:  "kb/technology/security/vulnerabilities/geospatial/geoserver/aaaaaa06.md",
			aTitle: "GeoServer property expression flaw under active exploitation",
			bTitle: "Active exploitation of a GeoServer property expression flaw",
			base:   4.0, delta: 0.2,
		},
	}

	placed := map[string][]float32{}
	for _, tw := range twins {
		placed[tw.aTitle] = axisVector(tw.base)
		placed[tw.bTitle] = axisVector(tw.base + tw.delta)
	}
	emb := &restatementEmbedder{vectorFor: func(text string) []float32 {
		return placed[text] // nil for anything else — the hash vector is used
	}}

	env := newRestatementEnvWith(t, 0, emb)
	// The filler first, and it is not decoration: both rarity cuts are read off
	// the corpus's own token distribution, so a corpus with no distribution has
	// nothing rare in it and no pair is ever classified.
	for i := range structuralFiller {
		env.writeFact(
			fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1),
			fmt.Sprintf("Filler note 2026 about widget %d", 1000+i),
			"an unrelated body")
	}
	// Then the twins, IN ORDER. This sequence is the mint order under test.
	for _, tw := range twins {
		env.writeFact(tw.aPath, tw.aTitle, "a body about the event")
		env.writeFact(tw.bPath, tw.bTitle, "the same event, filed again")
	}
	return env
}

// orderOf renders a pair list as a comparable order key.
func orderOf(pairs []store.RestatementPair) string {
	keys := make([]string, 0, len(pairs))
	for _, p := range pairs {
		keys = append(keys, pathPairKey(p.APath, p.BPath))
	}
	return strings.Join(keys, "|")
}

// isDescendingByCos reports whether a pair list is sorted by descending title
// cosine — i.e. whether it is the RANKING rather than the sweep.
func isDescendingByCos(pairs []store.RestatementPair) bool {
	for i := 1; i < len(pairs); i++ {
		if pairs[i].TitleCos > pairs[i-1].TitleCos {
			return false
		}
	}
	return true
}
