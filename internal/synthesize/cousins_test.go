package synthesize

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// Cousin-meeting inside prune (knomit#149).
//
// The fixture below is the measured shape, not an invented one: two facts about
// ONE event, filed under two sibling freeform categories, with a blended cosine
// ABOVE the model's mechanical dedup floor — and structurally unable to
// co-cluster because ScopedCluster fences each seed's neighbour search to its
// own directory.

const (
	cousinAPath = "kb/technology/security/vulnerabilities/network-security/check-point-cve-2026-16232.md"
	cousinBPath = "kb/technology/security/vulnerabilities/network-appliances/23d98f38.md"
	cousinTitle = "SmartConsole authentication bypass under active exploitation"
	cousinBody  = "The same event, recorded twice under two freeform categories."
	// A's SIBLING — same directory, close on the axis, so A lands in a
	// multi-fact cluster and therefore in a prune item.
	//
	// This is not fixture padding, it is the MEASURED SHAPE. In every one of
	// the six confirmed cousin pairs on the live corpus, one half was served in
	// a prune item and the other sat in the leftover bucket; that asymmetry is
	// what the sweep exploits, since it can only search outward from facts that
	// are already in a prune cluster. See
	// TestCousins_BothHalvesLeftoverIsNotReached for the case this does NOT
	// cover.
	cousinSiblingPath  = "kb/technology/security/vulnerabilities/network-security/smartconsole-followup.md"
	cousinSiblingTitle = "SmartConsole bypass: vendor confirms exploitation in the wild"
)

// cousinEnv places the twins on the axis by hand so the test controls the
// cosine exactly, and files them as COUSINS: `network-security` and
// `network-appliances` are siblings, so neither path is a prefix of the other
// and the fence makes them mutually unreachable.
func cousinEnv(t *testing.T, filler int) *restatementEnv {
	t.Helper()
	titleB := cousinTitle + " (weekly recap)"
	bodyB := cousinBody + " Filed a second time."
	emb := &restatementEmbedder{vectorFor: func(text string) []float32 {
		switch text {
		case cousinTitle:
			return axisVector(0)
		case titleB:
			return axisVector(0.02)
		case cousinTitle + " " + cousinBody:
			return axisVector(0.01)
		case titleB + " " + bodyB:
			return axisVector(0.03)
		case cousinSiblingTitle:
			return axisVector(0.04)
		case cousinSiblingTitle + " " + cousinBody:
			return axisVector(0.05)
		}
		return nil
	}}
	env := newRestatementEnvWith(t, 0, emb)
	for i := range filler {
		env.writeFact(
			fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1),
			fmt.Sprintf("Filler note 2026 about widget %d", 1000+i),
			"an unrelated body")
	}
	env.writeFact(cousinAPath, cousinTitle, cousinBody)
	env.writeFact(cousinSiblingPath, cousinSiblingTitle, cousinBody)
	env.writeFact(cousinBPath, titleB, bodyB)
	return env
}

// cousinFixtureIsSound asserts the two things the fixture CLAIMS, because a
// fixture chosen to make a test discriminate is a claim and an unasserted claim
// is how a test ends up measuring nothing (the campaign's fixture-vacuity
// lesson).
func cousinFixtureIsSound(t *testing.T, env *restatementEnv) {
	t.Helper()
	// (1) The pair really is above the mechanical dedup floor. Below it, the
	// search in joinCousinsForPrune would not return the cousin at all and
	// every test here would pass or fail for the wrong reason.
	ids := env.liveFactIDs()
	vecs, err := env.svc.Abstraction().BodyVectorsByFactID(context.Background(),
		[]int64{ids[cousinAPath], ids[cousinBPath]})
	require.NoError(t, err)
	cos := store.CosineSim(vecs[ids[cousinAPath]], vecs[ids[cousinBPath]])
	require.GreaterOrEqual(t, cos, env.dedupThreshold(),
		"fixture must sit at or above the mechanical dedup floor")

	// (2) They really are COUSINS — neither path is a prefix of the other, so
	// the fence genuinely separates them. A fixture where one path happened to
	// contain the other would be testing siblings, which already meet.
	a, b := categoryDir(cousinAPath), categoryDir(cousinBPath)
	require.False(t, strings.HasPrefix(a, b), "fixture paths must not nest")
	require.False(t, strings.HasPrefix(b, a), "fixture paths must not nest")
}

// TestCousins_FenceKeepsANonSeedCousinOutOfTheSubgraph is the PREMISE of this
// file, stated as the thing the fence provably does.
//
// It is deliberately NOT written as "run ScopedCluster over everything and
// check the two are apart". That version passes VACUOUSLY here — at the
// production resolution this fixture yields zero clusters, so "not together" is
// trivially true and the assertion measures nothing. (It was written that way
// first and passed for exactly that reason.)
//
// What the fence actually guarantees is narrower and checkable: a cousin that
// is not itself a seed can never ENTER the subgraph, because the only way in is
// some seed's neighbour search and every one of those is fenced to its own
// category directory. Absent from the subgraph, absent from every cluster, at
// any cosine.
func TestCousins_FenceKeepsANonSeedCousinOutOfTheSubgraph(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)
	cousinFixtureIsSound(t, env)

	// Seeds = everything EXCEPT the cousin. This is the measured shape: the
	// second twin sat in the leftover bucket, not in the seed set that built
	// any prune cluster.
	var seeds []factForLLM
	for _, f := range allFactsForLLM(t, env) {
		if f.File != cousinBPath {
			seeds = append(seeds, f)
		}
	}
	require.NotEmpty(t, seeds)

	clusters, err := ScopedCluster(ctx, seeds, env.svc.Search(),
		env.ri.ClusterResolution(), env.ri.ClusterMinCommunitySize(), func(ProgressEvent) {}, env.branch)
	require.NoError(t, err)
	for _, c := range clusters {
		for _, f := range c {
			require.NotEqual(t, cousinBPath, f.File,
				"PREMISE OF THIS FILE: a non-seed cousin cannot reach the subgraph, "+
					"because every route in is a neighbour search fenced to the "+
					"seed's own category directory. If this ever fails the blinder "+
					"is gone, and these tests must be re-derived rather than deleted")
		}
	}

	// And the unfenced search — the one this fix adds — DOES reach it. Without
	// this half the test above is satisfied by a corpus where the cousin is
	// simply not findable at all.
	hits, err := env.svc.Search().Search(ctx, env.branch, store.SearchOptions{
		QueryByPath:   cousinAPath,
		MinSimilarity: env.dedupThreshold(),
		Limit:         10,
	})
	require.NoError(t, err)
	var reachable bool
	for _, r := range hits {
		if r.Path == cousinBPath {
			reachable = true
		}
	}
	require.True(t, reachable,
		"the cousin must be reachable once the path fence is dropped — otherwise "+
			"the fence is not what was hiding it")
}

// TestCousins_JoinBringsThemTogetherForPrune is the fix.
func TestCousins_JoinBringsThemTogetherForPrune(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)
	cousinFixtureIsSound(t, env)
	d := env.deps()

	// The cousin sits OUTSIDE any prune cluster — the measured "leftover
	// bucket" case: one twin served in a prune item, the other never served
	// alongside it.
	pruneClusters := [][]factForLLM{{
		{File: cousinAPath, Title: cousinTitle},
		{File: "kb/technology/filler/topic0/00000001.md", Title: "Filler note 2026 about widget 1000"},
	}}
	require.False(t, clustersHoldTogether(pruneClusters, cousinAPath, cousinBPath),
		"fixture must START with the pair apart, or the join is credited for nothing")

	joined, h := joinCousinsForPrune(ctx, d, env.branch, pruneClusters, env.dedupThreshold())
	require.Empty(t, h.Failure)
	require.True(t, clustersHoldTogether(joined, cousinAPath, cousinBPath),
		"a near-certain duplicate the category fence hid must be put in front of "+
			"the prune judge")
	require.Positive(t, h.Attached, "the health must report the attachment it made")
}

// TestCousins_JoinUnionsTwoPruneClusters is the other arrangement, and it is a
// separate code path.
//
// When BOTH cousins already sit in prune clusters — different ones — attaching
// is not enough; the two clusters have to be merged, or the judge still sees
// them in separate work items and can still never merge them.
func TestCousins_JoinUnionsTwoPruneClusters(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)
	cousinFixtureIsSound(t, env)

	pruneClusters := [][]factForLLM{
		{{File: cousinAPath}, {File: "kb/technology/filler/topic0/00000001.md"}},
		{{File: cousinBPath}, {File: "kb/technology/filler/topic1/00000002.md"}},
	}
	joined, h := joinCousinsForPrune(ctx, env.deps(), env.branch, pruneClusters, env.dedupThreshold())
	require.Empty(t, h.Failure)
	require.Positive(t, h.Joined, "two prune clusters linked by a cousin must be merged")
	require.True(t, clustersHoldTogether(joined, cousinAPath, cousinBPath),
		"a pair split across two prune ITEMS is as unjudgeable as one split across clusters")
	require.Len(t, joined, 1, "the two clusters become one")
}

// TestCousins_ClustersAreNotMutated is the blast-radius guarantee, and it is
// the load-bearing half of "augment, not replace".
//
// `clusters` is read by FOUR other consumers after this pass runs —
// dedupCluster (already done), distillGroups, clusterResultFromGroups (both
// bridge axes and discover), and planRestatementShortlist's co-membership. The
// ruling scopes #149 to prune, so a cousin must never reach any of them.
func TestCousins_ClustersAreNotMutated(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)
	cousinFixtureIsSound(t, env)

	original := [][]factForLLM{{
		{File: cousinAPath, Title: cousinTitle},
		{File: "kb/technology/filler/topic0/00000001.md", Title: "Filler note 2026 about widget 1000"},
	}}
	before := renderClusters(original)

	joined, h := joinCousinsForPrune(ctx, env.deps(), env.branch, original, env.dedupThreshold())
	require.Positive(t, h.Attached, "fixture must actually attach something, or immutability is trivial")
	require.True(t, clustersHoldTogether(joined, cousinAPath, cousinBPath))

	require.Equal(t, before, renderClusters(original),
		"the input cluster slice must be untouched — distill, both bridge axes "+
			"and discover read it AFTER this runs, and the ruling scopes this "+
			"fix to prune alone")
}

// TestCousins_SiblingsAreNotDoubleCounted — facts already in one cluster meet
// by construction, and re-adding them would put the same fact in a prune item
// twice, asking the judge to compare a fact with itself.
func TestCousins_SiblingsAreNotDoubleCounted(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)

	// Both twins in ONE cluster already.
	pruneClusters := [][]factForLLM{{{File: cousinAPath}, {File: cousinBPath}}}
	joined, h := joinCousinsForPrune(ctx, env.deps(), env.branch, pruneClusters, env.dedupThreshold())
	require.Zero(t, h.Joined, "a pair already in one cluster needs no join")
	require.Len(t, joined, 1)
	seen := map[string]int{}
	for _, f := range joined[0] {
		seen[f.File]++
	}
	for path, n := range seen {
		require.Equal(t, 1, n, "fact %s appears %d times in one prune item", path, n)
	}
}

// TestCousins_TruncatedSweepSaysSo — a cap that nothing reports reads as
// "covered everything". The whole failure class this area keeps hitting is caps
// that stay silent, so a truncated sweep must say which facts it did NOT reach.
func TestCousins_TruncatedSweepSaysSo(t *testing.T) {
	full := cousinHealth{Searched: maxCousinSearches, Candidate: maxCousinSearches + 7, Truncated: true}
	line := cousinSignalLine(full)
	require.Contains(t, line, "SWEEP TRUNCATED")
	require.Contains(t, line, "7 prune facts were not swept",
		"the line must name HOW MANY went unswept, or a reader cannot size the gap")

	complete := cousinHealth{Searched: 12, Candidate: 12}
	require.NotContains(t, cousinSignalLine(complete), "TRUNCATED")
}

// TestCousins_BudgetActuallyTruncates drives the real function, because the
// test above asserts on a health struct the test itself built — which pins the
// formatter and not the cap.
func TestCousins_BudgetActuallyTruncates(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, maxCousinSearches+10)

	// One cluster holding more facts than the search budget allows.
	var big []factForLLM
	for i := range maxCousinSearches + 5 {
		big = append(big, factForLLM{File: fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", i, i+1)})
	}
	require.Greater(t, len(big), maxCousinSearches,
		"fixture must exceed the budget, or truncation is never reached")

	_, h := joinCousinsForPrune(ctx, env.deps(), env.branch, [][]factForLLM{big}, env.dedupThreshold())
	require.True(t, h.Truncated, "a sweep over more facts than the budget must report truncation")
	require.Equal(t, maxCousinSearches, h.Searched, "and it must stop AT the budget")
	require.Equal(t, len(big), h.Candidate, "and still report what a complete sweep would have been")
}

// TestCousins_TruncatedSweepIsIndependentOfClusterOrder pins what the sweep's
// sort actually buys.
//
// A first version of this test just called the function repeatedly and compared
// runs. That passes with the sort DELETED (sabotage S15): `targets` is
// collected from a slice, so a single run is already reproducible without it.
// Repetition alone cannot see the property.
//
// What the sort guarantees is that the truncated tail depends on the SET of
// prune facts and not on the ORDER Louvain emitted its communities in. So the
// test feeds the same facts grouped into the same clusters, PRESENTED IN
// REVERSED ORDER, and requires the swept set to be identical. Without the sort
// the two presentations sweep different subsets and the prune items differ for
// one unchanged corpus.
func TestCousins_TruncatedSweepIsIndependentOfClusterOrder(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, maxCousinSearches+10)

	// The ONE cluster that can attach a cousin, plus enough filler clusters to
	// blow the search budget. Filler paths (kb/technology/filler/…) sort BEFORE
	// the cousin's (kb/technology/security/…), so under the sort the cousin
	// falls outside the budget in BOTH presentations. Without the sort its fate
	// depends purely on which end of the list its cluster sits at.
	cousinCluster := []factForLLM{{File: cousinAPath}}
	clusters := [][]factForLLM{cousinCluster}
	for c := range 3 {
		var cluster []factForLLM
		for i := range maxCousinSearches / 2 {
			n := c*(maxCousinSearches/2) + i
			cluster = append(cluster, factForLLM{
				File: fmt.Sprintf("kb/technology/filler/topic%d/%08x.md", n, n+1)})
		}
		clusters = append(clusters, cluster)
	}
	total := 0
	for _, c := range clusters {
		total += len(c)
	}
	require.Greater(t, total, maxCousinSearches,
		"fixture must exceed the budget, or there is no tail to truncate and "+
			"cluster order cannot matter")

	// Fixture assertion: on its own — no budget pressure — that cluster DOES
	// attach the cousin. Without this, "0 attached" below would be satisfied by
	// a corpus that simply has no cousin to find, and the test would pass while
	// measuring nothing.
	_, solo := joinCousinsForPrune(ctx, env.deps(), env.branch, [][]factForLLM{cousinCluster}, env.dedupThreshold())
	require.Positive(t, solo.Attached,
		"fixture's cousin cluster must attach when the budget is not the constraint")

	_, forward := joinCousinsForPrune(ctx, env.deps(), env.branch, clusters, env.dedupThreshold())
	require.True(t, forward.Truncated, "fixture must truncate, or this measures nothing")

	reversed := make([][]factForLLM, 0, len(clusters))
	for i := len(clusters) - 1; i >= 0; i-- {
		reversed = append(reversed, clusters[i])
	}
	_, backward := joinCousinsForPrune(ctx, env.deps(), env.branch, reversed, env.dedupThreshold())

	require.Equal(t, forward.Searched, backward.Searched)
	require.Equal(t, forward.Attached, backward.Attached,
		"the same facts in a different cluster ORDER must sweep the same subset — "+
			"otherwise one unchanged corpus produces different prune items "+
			"depending only on the sequence Louvain emitted its communities in")
}

// TestCousins_PlanRunsTheSweepAndReportsIt is the WIRING half.
//
// Calling joinCousinsForPrune asserts joinCousinsForPrune works, not that
// anything calls it (the campaign's path-vs-state rule). This drives the real
// Plan through StartSession and reads what the session says about itself.
//
// It asserts the pass RAN and REPORTED — not that a cousin was attached —
// because whether this fixture yields any prune cluster at all depends on the
// production Louvain resolution, which is a property of the clusterer rather
// than of this fix. The attachment itself is pinned by the unit tests above,
// against prune clusters built the way Plan builds them.
func TestCousins_PlanRunsTheSweepAndReportsIt(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)
	cousinFixtureIsSound(t, env)

	p := NewPipeline(env.ri, func(ProgressEvent) {}, EffortNormal, ScopeFilter{}, reviewStrategy{})
	res, err := p.StartSession(ctx)
	require.NoError(t, err)

	require.Contains(t, strings.Join(res.Health, "\n"), "cross-category prune:",
		"the pass must report itself even when it finds nothing — silence is how "+
			"the category fence stayed invisible for as long as it did, and an "+
			"unreported pass is indistinguishable from one that never ran")
}

// TestCousins_BothHalvesLeftoverIsNotReached records a LIMIT of this fix, so it
// is a known boundary rather than a surprise.
//
// The sweep searches outward from facts that are ALREADY in a prune cluster. A
// cousin pair where NEITHER half is in one — both sitting in the leftover
// bucket — has no origin to search from and is not reached.
//
// That is the ruled scope ("cousin-meeting INSIDE prune") and it covers the
// measured population: in all six confirmed pairs on the live corpus, one half
// was served in a prune item and the other was the leftover. Widening the sweep
// origin to every seed would also CREATE prune items where there were none,
// which is a larger behaviour change than the one ruled — so it is recorded
// here rather than done quietly.
//
// If this test ever fails, the sweep origin was widened: that may be correct,
// but it is a scope change and should be a decision, not a side effect.
func TestCousins_BothHalvesLeftoverIsNotReached(t *testing.T) {
	ctx := context.Background()
	env := cousinEnv(t, 20)
	cousinFixtureIsSound(t, env)

	// A prune cluster made of two UNRELATED filler facts: neither cousin is in
	// it, so neither is a sweep origin.
	pruneClusters := [][]factForLLM{{
		{File: "kb/technology/filler/topic0/00000001.md"},
		{File: "kb/technology/filler/topic1/00000002.md"},
	}}
	joined, h := joinCousinsForPrune(ctx, env.deps(), env.branch, pruneClusters, env.dedupThreshold())
	require.Empty(t, h.Failure)
	require.False(t, clustersHoldTogether(joined, cousinAPath, cousinBPath),
		"KNOWN LIMIT: with neither half in a prune cluster there is no origin to "+
			"sweep from, so this pair is not reached. Widening the origin to every "+
			"seed is a scope change, not a bug fix")
}

// allFactsForLLM reads every live fact as a seed, the way Plan does.
func allFactsForLLM(t *testing.T, env *restatementEnv) []factForLLM {
	t.Helper()
	live, err := env.svc.Abstraction().LiveEpistemicFacts(context.Background(), env.branch)
	require.NoError(t, err)
	out := make([]factForLLM, 0, len(live))
	for _, path := range live {
		out = append(out, factForLLM{File: path})
	}
	return out
}

// clustersHoldTogether reports whether any one cluster contains both paths.
func clustersHoldTogether(clusters [][]factForLLM, a, b string) bool {
	for _, c := range clusters {
		var hasA, hasB bool
		for _, f := range c {
			if f.File == a {
				hasA = true
			}
			if f.File == b {
				hasB = true
			}
		}
		if hasA && hasB {
			return true
		}
	}
	return false
}

// renderClusters is a stable string form for equality assertions.
func renderClusters(clusters [][]factForLLM) string {
	var b strings.Builder
	for _, c := range clusters {
		for _, f := range c {
			b.WriteString(f.File)
			b.WriteString(",")
		}
		b.WriteString("|")
	}
	return b.String()
}
