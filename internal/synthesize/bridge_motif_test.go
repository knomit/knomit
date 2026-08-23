package synthesize

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/store"
)

// identityResolver is the alias table a corpus has before any judge pass: every
// spelling is its own cluster.
func identityResolver(m string) string { return m }

// constDF answers every df query with n — used where the df band is not what
// the test is about.
func constDF(n int) motifDFFn { return func(string) int { return n } }

// twoCommunities places each path in its own cluster, which is what makes a
// pair a BRIDGE rather than an in-cluster neighbour.
func twoCommunities(a, b string) ClusterResult {
	return ClusterResult{Clusters: map[int][]string{0: {a}, 1: {b}}}
}

func TestEnumerateMotifCandidates_GroupsOnCanonicalIDAcrossSpellings(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/gotchas/uitesting/1.md", Motifs: []string{"measure-becomes-target"},
			Entities: []string{"Cognition"}},
		{File: "kb/technology/rewardhacking/2.md", Motifs: []string{"metric-becomes-target"},
			Entities: []string{"RLHF"}},
	}
	// The alias judge merged the two spellings; enumeration keys on the result.
	resolve := func(string) string { return "measure-becomes-target" }

	got, h := enumerateMotifCandidates(seeds,
		twoCommunities("kb/gotchas/uitesting/1.md", "kb/technology/rewardhacking/2.md"),
		resolve, constDF(2), labelsWith(200, map[string]int{"cognition": 2, "rlhf": 3}), tierExact)

	require.Len(t, got, 1)
	require.Equal(t, "measure-becomes-target", got[0].Token, "Token is the CANONICAL id")
	require.Equal(t, BridgeMotif, got[0].Kind)
	require.Len(t, got[0].Members, 2)
	require.Equal(t, 1, h.Candidates)
}

func TestEnumerateMotifCandidates_DFBandExcludesBothEnds(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"hapax-shape", "generic-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"hapax-shape", "generic-shape"}, Entities: []string{"Beta"}},
	}
	// N=200 => ceiling max(12, 2%*200) = 12. One motif below the floor, one above.
	df := func(c string) int {
		if c == "hapax-shape" {
			return 1
		}
		return 40
	}

	got, h := enumerateMotifCandidates(seeds, twoCommunities("kb/alpha/1.md", "kb/beta/2.md"),
		identityResolver, df, labelsWith(200, nil), tierExact)

	require.Empty(t, got, "df=1 cannot bridge yet; df=40 has gone generic")
	require.Equal(t, 12, h.Ceiling)
	require.Equal(t, []string{"generic-shape"}, h.OverCeilingNames,
		"over-ceiling is FLAGGED for review splitting, not silently dropped (MN8)")
}

func TestEnumerateMotifCandidates_DFCeilingScalesWithTheCorpus(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
	}
	clusters := twoCommunities("kb/alpha/1.md", "kb/beta/2.md")

	// df 30 is over the ceiling on a 200-fact corpus (12) and under it on a
	// 3000-fact one (60): the band is a property of the corpus, not a constant.
	small, hs := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(30),
		labelsWith(200, nil), tierExact)
	large, hl := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(30),
		labelsWith(3000, nil), tierExact)

	require.Equal(t, 12, hs.Ceiling)
	require.Equal(t, 60, hl.Ceiling)
	require.Empty(t, small)
	require.Len(t, large, 1)
}

func TestEnumerateMotifCandidates_RequiresTwoCommunities(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
	}
	// Both in one community: separation 1. A GATE, never a reward (1f536807/MN8).
	same := ClusterResult{Clusters: map[int][]string{0: {"kb/alpha/1.md", "kb/beta/2.md"}}}

	got, _ := enumerateMotifCandidates(seeds, same, identityResolver, constDF(2),
		labelsWith(200, nil), tierExact)
	require.Empty(t, got)
}

// MN7's fixture asserted at the enumeration boundary the consumer actually
// reads, not only at the gate helper.
func TestEnumerateMotifCandidates_DropsTheNearDuplicatePair(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/technology/ai/agents/1.md", Motifs: []string{"shared-shape"},
			Entities: []string{"Cognition", "Devin"}},
		{File: "kb/technology/ai/agents/2.md", Motifs: []string{"shared-shape"},
			Entities: []string{"Cognition", "Devin"}},
	}
	labels := labelsWith(200, map[string]int{
		"cognition": 2, "devin": 2, "agent": 30, "technology": 40, "ai": 35})

	got, _ := enumerateMotifCandidates(seeds,
		twoCommunities("kb/technology/ai/agents/1.md", "kb/technology/ai/agents/2.md"),
		identityResolver, constDF(2), labels, tierExact)

	require.Empty(t, got, "MN7: the axis must not re-find the 8ebd5d90 population")
}

// The complement: the same shape, subject-disjoint, DOES enumerate. Without
// this the test above would pass on a gate that rejected everything.
func TestEnumerateMotifCandidates_KeepsTheSubjectDisjointPair(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/gotchas/uitesting/1.md", Motifs: []string{"shared-shape"},
			Entities: []string{"Cognition"}, Domain: []string{"evaluation"}},
		{File: "kb/technology/rewardhacking/2.md", Motifs: []string{"shared-shape"},
			Entities: []string{"RLHF"}, Domain: []string{"evaluation"}},
	}
	labels := labelsWith(200, map[string]int{"evaluation": 30, "cognition": 2, "rlhf": 3})

	got, _ := enumerateMotifCandidates(seeds,
		twoCommunities("kb/gotchas/uitesting/1.md", "kb/technology/rewardhacking/2.md"),
		identityResolver, constDF(2), labels, tierExact)

	require.Len(t, got, 1, "one shared coarse tag must not block a genuine bridge")
}

// A group of three where ONE member shares a subject with another: the offender
// drops, the bridge survives. All-or-nothing rejection would let a single
// near-duplicate delete a genuine three-way group.
func TestEnumerateMotifCandidates_DropsOffendingMemberNotTheGroup(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
		// Same rare entity as the first member — a near-duplicate of it.
		{File: "kb/alpha/3.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
	}
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/alpha/1.md"}, 1: {"kb/beta/2.md"}, 2: {"kb/alpha/3.md"}}}
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2})

	got, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(3), labels, tierExact)

	require.Len(t, got, 1)
	require.Len(t, got[0].Members, 2, "the third member is the first one's near-duplicate")
	require.Equal(t, "kb/alpha/1.md", got[0].Members[0].File, "path-sorted, so the survivor is deterministic")
	require.Equal(t, "kb/beta/2.md", got[0].Members[1].File)
}

func TestEnumerateMotifCandidates_Token2TierGroupsDistinctCanonicalIDs(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"stale-cache-capture"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"cache-capture-drift"}, Entities: []string{"Beta"}},
	}
	clusters := twoCommunities("kb/alpha/1.md", "kb/beta/2.md")
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2})

	exact, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierExact)
	require.Empty(t, exact, "distinct canonical ids do not match verbatim")

	loose, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierToken2)
	require.Len(t, loose, 1, "two shared stemmed tokens (cache, capture) match at token-2")
	require.Len(t, loose[0].Members, 2)
}

func TestEnumerateMotifCandidates_Token2NeedsTwoSharedTokens(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"stale-cache-capture"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"cache-warming-strategy"}, Entities: []string{"Beta"}},
	}
	got, _ := enumerateMotifCandidates(seeds, twoCommunities("kb/alpha/1.md", "kb/beta/2.md"),
		identityResolver, constDF(2), labelsWith(200, map[string]int{"alpha": 2, "beta": 2}), tierToken2)
	require.Empty(t, got, "one shared token (cache) is below the token-2 tier")
}

// §7 idempotency (cf455b8f): discovery never feeds on its own output.
func TestEnumerateMotifCandidates_ExcludesDiscoveredOrigin(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"},
			Origin: "discovered"},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
	}
	got, _ := enumerateMotifCandidates(seeds, twoCommunities("kb/alpha/1.md", "kb/beta/2.md"),
		identityResolver, constDF(2), labelsWith(200, map[string]int{"alpha": 2, "beta": 2}), tierExact)
	require.Empty(t, got)
}

func TestEnumerateMotifCandidates_IsDeterministic(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"bravo-shape", "alpha-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"bravo-shape", "alpha-shape"}, Entities: []string{"Beta"}},
	}
	clusters := twoCommunities("kb/alpha/1.md", "kb/beta/2.md")
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2})

	first, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierExact)
	for i := 0; i < 20; i++ {
		again, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, tierExact)
		require.Equal(t, first, again, "map iteration order must not reach the output")
	}
	require.Equal(t, "alpha-shape", first[0].Token, "token-sorted")
}

// ── lane split and per-lane scoring (§4) ──────────────────────────────────

func TestLaneOf_SplitsOnSimilarityAdjacency(t *testing.T) {
	paths := []string{"kb/alpha/1.md", "kb/beta/2.md"}
	near := store.NewSimilarityGraph([][2]string{{"kb/alpha/1.md", "kb/beta/2.md"}})
	far := store.NewSimilarityGraph(nil)

	require.Equal(t, LaneNear, laneOf(paths, near))
	require.Equal(t, LaneFar, laneOf(paths, far),
		"no SIMILAR_TO edge means cohesion 0 by construction — the far lane")
}

// motifScoreEnv builds the gomock index a scoring test needs: no DERIVED_FROM
// links between members (Gap = 1.0), and no TokenDF call, since the shared-motif
// specificity is passed in already summed.
func motifScoreEnv(t *testing.T, paths ...string) (context.Context, *MockSearchIndex) {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	idx := NewMockSearchIndex(ctrl)
	for _, p := range paths {
		idx.EXPECT().ReverseDependentPaths(gomock.Any(), p).
			Return(map[string]struct{}{}, nil).AnyTimes()
	}
	return context.Background(), idx
}

func motifCandidate() BridgeSeedSet {
	return BridgeSeedSet{Token: "shared-shape", Kind: BridgeMotif, Members: []factForLLM{
		{File: "kb/alpha/1.md"}, {File: "kb/beta/2.md"}}}
}

func motifQualityConfig() QualityConfig {
	return QualityConfig{CohFloor: 0.5, QualityFloor: 0, WCoh: 1, WGap: 1, WSpec: 1, MaxMembers: 5}
}

// TestScoreMotifCandidate_FarLaneRewardsDissimilarity — the far lane's whole
// claim: the LESS similar the members, the more work the shared mechanism is
// doing, so the score must move the other way from the near lane's cohesion.
func TestScoreMotifCandidate_FarLaneRewardsDissimilarity(t *testing.T) {
	ctx, idx := motifScoreEnv(t, "kb/alpha/1.md", "kb/beta/2.md")
	g := store.NewSimilarityGraph(nil)
	clusterOf := map[string]int{"kb/alpha/1.md": 0, "kb/beta/2.md": 1}

	distant, keptA, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar, g, idx, "main",
		clusterOf, motifQualityConfig(), 0.5, constMeanSim(0.1))
	require.NoError(t, err)
	require.True(t, keptA,
		"the far lane must not apply the cohesion floor — cohesion is 0 there by construction")

	close, keptB, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar, g, idx, "main",
		clusterOf, motifQualityConfig(), 0.5, constMeanSim(0.9))
	require.NoError(t, err)
	require.True(t, keptB)

	require.Greater(t, distant, close)
}

// The near lane keeps the existing gate untouched, and a near-lane group below
// the cohesion floor is DROPPED — not re-routed into the far lane. The lanes
// partition the candidates; they are not a retry.
func TestScoreMotifCandidate_NearLaneKeepsTheCohesionFloor(t *testing.T) {
	ctx, idx := motifScoreEnv(t, "kb/alpha/1.md", "kb/beta/2.md")
	g := store.NewSimilarityGraph(nil) // density 0, below the floor

	_, kept, err := scoreMotifCandidate(ctx, motifCandidate(), LaneNear, g, idx, "main",
		map[string]int{"kb/alpha/1.md": 0, "kb/beta/2.md": 1}, motifQualityConfig(), 0.5, nil)
	require.NoError(t, err)
	require.False(t, kept)
}

// MN8: separation >= 2 is a gate in BOTH lanes. The far lane replaces the
// cohesion term, not the separation gate.
func TestScoreMotifCandidate_SeparationIsAGateInBothLanes(t *testing.T) {
	ctx, idx := motifScoreEnv(t, "kb/alpha/1.md", "kb/beta/2.md")
	sameCommunity := map[string]int{"kb/alpha/1.md": 0, "kb/beta/2.md": 0}
	cfg := motifQualityConfig()
	cfg.CohFloor = 0 // so only separation can be what rejects

	_, keptFar, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar,
		store.NewSimilarityGraph(nil), idx, "main", sameCommunity, cfg, 0.5, constMeanSim(0))
	require.NoError(t, err)
	require.False(t, keptFar)

	_, keptNear, err := scoreMotifCandidate(ctx, motifCandidate(), LaneNear,
		store.NewSimilarityGraph([][2]string{{"kb/alpha/1.md", "kb/beta/2.md"}}),
		idx, "main", sameCommunity, cfg, 0.5, nil)
	require.NoError(t, err)
	require.False(t, keptNear)
}

// The far lane is size-capped like the near lane. Oversized far groups are
// DROPPED here rather than trimmed: the §4 trim ("maximum community spread,
// then minimum mean similarity") is carried forward to Phase 4 by designer
// ruling, and dropping is the conservative behaviour in the meantime.
func TestScoreMotifCandidate_FarLaneDropsOversizedGroups(t *testing.T) {
	paths := []string{"kb/a/1.md", "kb/b/2.md", "kb/c/3.md", "kb/d/4.md"}
	ctx, idx := motifScoreEnv(t, paths...)
	cand := BridgeSeedSet{Token: "shared-shape", Kind: BridgeMotif}
	clusterOf := map[string]int{}
	for i, p := range paths {
		cand.Members = append(cand.Members, factForLLM{File: p})
		clusterOf[p] = i
	}
	cfg := motifQualityConfig()
	cfg.MaxMembers = 3

	_, kept, err := scoreMotifCandidate(ctx, cand, LaneFar, store.NewSimilarityGraph(nil),
		idx, "main", clusterOf, cfg, 0.5, constMeanSim(0.1))
	require.NoError(t, err)
	require.False(t, kept)
}

// A far-lane group whose members carry no vectors must not read as maximally
// dissimilar — that would hand every unembedded group the top score. The
// caller's meanSim contract says so; this pins the scorer's half of it.
func TestScoreMotifCandidate_FarLanePropagatesMeanSimErrors(t *testing.T) {
	ctx, idx := motifScoreEnv(t, "kb/alpha/1.md", "kb/beta/2.md")
	_, _, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar, store.NewSimilarityGraph(nil),
		idx, "main", map[string]int{"kb/alpha/1.md": 0, "kb/beta/2.md": 1},
		motifQualityConfig(), 0.5,
		func(context.Context, []string) (float64, error) { return 0, errors.New("no vectors") })
	require.Error(t, err, "an unreadable similarity is not a licence to score the group high")
}

func constMeanSim(v float64) meanSimFn {
	return func(context.Context, []string) (float64, error) { return v, nil }
}

// ── shared-motif specificity: a rank boost, never a gate (§4) ─────────────

func TestSharedMotifSpecificity_SecondSharedMotifBoostsNeverGates(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", gomock.Any(), string(BridgeMotif)).
		Return(2, nil).AnyTimes()

	one := BridgeSeedSet{Token: "alpha-shape", Members: []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"alpha-shape"}},
		{File: "kb/beta/2.md", Motifs: []string{"alpha-shape"}}}}
	two := BridgeSeedSet{Token: "alpha-shape", Members: []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"alpha-shape", "bravo-shape"}},
		{File: "kb/beta/2.md", Motifs: []string{"alpha-shape", "bravo-shape"}}}}

	sOne, err := sharedMotifSpecificity(ctx, idx, "main", one, identityResolver)
	require.NoError(t, err)
	sTwo, err := sharedMotifSpecificity(ctx, idx, "main", two, identityResolver)
	require.NoError(t, err)

	require.Greater(t, sTwo, sOne, "a second shared motif raises the score")
	require.Positive(t, sOne, "and one shared motif still scores — it is never a gate")
}

func TestSharedMotifSpecificity_OnlyMotifsEveryMemberCarriesCount(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", gomock.Any(), string(BridgeMotif)).
		Return(2, nil).AnyTimes()

	cand := BridgeSeedSet{Token: "alpha-shape", Members: []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"alpha-shape", "bravo-shape"}},
		{File: "kb/beta/2.md", Motifs: []string{"alpha-shape"}}}}

	got, err := sharedMotifSpecificity(ctx, idx, "main", cand, identityResolver)
	require.NoError(t, err)
	require.InDelta(t, 0.5, got, 1e-9, "bravo-shape is not carried by ALL members")
}

// Rarity is what the term measures: a motif on fewer facts contributes more.
func TestSharedMotifSpecificity_IsInverseInDF(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", "rare-shape", string(BridgeMotif)).Return(2, nil)
	idx.EXPECT().TokenDF(gomock.Any(), "main", "common-shape", string(BridgeMotif)).Return(10, nil)

	mk := func(m string) BridgeSeedSet {
		return BridgeSeedSet{Token: m, Members: []factForLLM{
			{File: "kb/alpha/1.md", Motifs: []string{m}}, {File: "kb/beta/2.md", Motifs: []string{m}}}}
	}
	rare, err := sharedMotifSpecificity(context.Background(), idx, "main", mk("rare-shape"), identityResolver)
	require.NoError(t, err)
	common, err := sharedMotifSpecificity(context.Background(), idx, "main", mk("common-shape"), identityResolver)
	require.NoError(t, err)
	require.Greater(t, rare, common)
}

// One member spelling the same cluster two ways must not vote twice — that
// would be a member reinforcing a group on its own authorship habit.
func TestSharedMotifSpecificity_OneMemberCannotVoteTwiceForACluster(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", "alpha-shape", string(BridgeMotif)).
		Return(2, nil).AnyTimes()

	// Both spellings resolve to one canonical id.
	resolve := func(string) string { return "alpha-shape" }
	cand := BridgeSeedSet{Token: "alpha-shape", Members: []factForLLM{
		{File: "kb/alpha/1.md", Motifs: []string{"alpha-shape", "alpha-shaped"}},
		{File: "kb/beta/2.md", Motifs: []string{"alpha-shape"}}}}

	got, err := sharedMotifSpecificity(context.Background(), idx, "main", cand, resolve)
	require.NoError(t, err)
	require.InDelta(t, 0.5, got, 1e-9, "one cluster, one term")
}

// ── effort binding and per-lane sub-budgets (§5) ──────────────────────────

func TestMotifSubBudget_IsMonotoneAndLaneBound(t *testing.T) {
	nN, fN := motifSubBudget(EffortNormal)
	nM, fM := motifSubBudget(EffortMedium)
	nH, fH := motifSubBudget(EffortHigh)

	require.Equal(t, 2, nN, "§5: normal's bounded 6ce866f8 amendment is a hard cap of 2")
	require.Zero(t, fN, "normal is near lane only")
	require.Equal(t, 4, nM)
	require.Zero(t, fM, "medium stays on the validated forward shape — never conjecture")
	require.Positive(t, fH, "the far lane opens only at high")

	// MN10: monotone in budget as well as in kind.
	require.LessOrEqual(t, nN, nM)
	require.LessOrEqual(t, nM, nH)
	require.LessOrEqual(t, fM, fH)
}

// MN8: the motif budgets are ADDITIONAL, never carved out of the entity/domain
// pool — the axes have different df distributions and one pool would let the
// densest starve the others.
func TestMotifSubBudget_DoesNotDrawOnTheEntityDomainBudget(t *testing.T) {
	require.Equal(t, 48, effortBudget(EffortHigh))
	require.Equal(t, 12, effortBudget(EffortMedium))
	require.Zero(t, effortBudget(EffortNormal))
}

func TestMotifTier_IsMonotoneInEffort(t *testing.T) {
	require.Equal(t, tierExact, motifTier(EffortNormal))
	require.Equal(t, tierExact, motifTier(EffortMedium))
	require.Equal(t, tierToken2, motifTier(EffortHigh))
}

// MN10 as a PROPERTY, not an arrangement: each level's candidate set is a
// subset of the next level's, before any budget truncation.
func TestMotifCandidates_AreMonotoneAcrossEffort(t *testing.T) {
	seeds := []factForLLM{
		// A verbatim pair — matches at every tier.
		{File: "kb/alpha/1.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/beta/2.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
		// A token-2-only pair — matches only at high.
		{File: "kb/gamma/3.md", Motifs: []string{"stale-cache-capture"}, Entities: []string{"Gamma"}},
		{File: "kb/delta/4.md", Motifs: []string{"cache-capture-drift"}, Entities: []string{"Delta"}},
	}
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/alpha/1.md"}, 1: {"kb/beta/2.md"}, 2: {"kb/gamma/3.md"}, 3: {"kb/delta/4.md"}}}
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2, "gamma": 2, "delta": 2})

	setOf := func(e Effort) map[string]struct{} {
		got, _ := enumerateMotifCandidates(seeds, clusters, identityResolver, constDF(2), labels, motifTier(e))
		out := map[string]struct{}{}
		for _, b := range got {
			for _, m := range b.Members {
				out[m.File] = struct{}{}
			}
		}
		return out
	}
	normal, medium, high := setOf(EffortNormal), setOf(EffortMedium), setOf(EffortHigh)

	require.NotEmpty(t, normal, "a vacuous subset assertion tests nothing")
	require.Subset(t, medium, normal)
	require.Subset(t, high, medium)
	require.Greater(t, len(high), len(normal), "high must actually admit more, or the ladder is flat")
}

// ── the builder ───────────────────────────────────────────────────────────

// countingSearchIndex records whether the builder touched the index at all.
type countingSearchIndex struct {
	SearchQuery
	calls int
}

func (c *countingSearchIndex) TokenDF(context.Context, string, string, string) (int, error) {
	c.calls++
	return 2, nil
}

func (c *countingSearchIndex) SimilarityAdjacency(context.Context, []string) (store.SimilarityGraph, error) {
	c.calls++
	return store.NewSimilarityGraph(nil), nil
}

// No DERIVED_FROM edges between members, so the derivation gap is 1.0 — nobody
// has already made the connection these candidates propose.
func (c *countingSearchIndex) ReverseDependentPaths(context.Context, string) (map[string]struct{}, error) {
	c.calls++
	return map[string]struct{}{}, nil
}

// MN5's own mechanism, asserted directly rather than left as a consequence of
// later gates: on a corpus with no motifs the tier does nothing and costs
// nothing — which is why the EffortNormal contract test passes vacuously.
func TestBuildMotifBridges_MotifFreeCorpusDoesNoWork(t *testing.T) {
	idx := &countingSearchIndex{}
	seeds := []factForLLM{{File: "kb/alpha/1.md"}, {File: "kb/beta/2.md"}}

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		twoCommunities("kb/alpha/1.md", "kb/beta/2.md"), EffortNormal, motifQualityConfig(),
		identityResolver, labelsWith(200, nil), nil)

	require.NoError(t, err)
	require.Empty(t, near)
	require.Empty(t, far)
	require.Zero(t, health.Candidates)
	require.Zero(t, idx.calls, "a motif-free corpus must not cost a single index call")
}

// saturatedMotifCorpus builds n verbatim-matching, subject-disjoint,
// cross-community pairs — more than any lane's budget.
func saturatedMotifCorpus(n int) ([]factForLLM, ClusterResult, store.SubjectLabelDF) {
	var seeds []factForLLM
	clusters := ClusterResult{Clusters: map[int][]string{}}
	labels := labelsWith(400, nil)
	com := 0
	for i := 0; i < n; i++ {
		// Motif tokens are unique per pair. Deliberate: at high effort the
		// token-2 tier merges any two canonical ids sharing two stemmed tokens,
		// so a family like "shape00-of-kind"/"shape01-of-kind" collapses into
		// ONE group on the connectives — spec-conformant (§4's permissive
		// prefilter) but not what a BUDGET test means to measure.
		motif := fmt.Sprintf("shape%02d-alpha%02d", i, i)
		for _, side := range []string{"a", "b"} {
			// Distinct FILENAMES as well as distinct directories: a fact's
			// whole path is a subject claim, so two facts both called "1.md"
			// share a subject token and the gate correctly rejects the pair.
			path := fmt.Sprintf("kb/%s%02d/f%s%02d.md", side, i, side, i)
			ent := fmt.Sprintf("ent%s%02d", side, i)
			seeds = append(seeds, factForLLM{File: path, Motifs: []string{motif},
				Entities: []string{ent}})
			clusters.Clusters[com] = []string{path}
			labels.DF[ent] = 2
			labels.DF[fmt.Sprintf("%s%02d", side, i)] = 2
			com++
		}
	}
	return seeds, clusters, labels
}

func TestBuildMotifBridges_NormalEmitsAtMostTwoNearAndNoFar(t *testing.T) {
	seeds, clusters, labels := saturatedMotifCorpus(6)
	idx := &allAdjacentIndex{} // every group is a near-lane group

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortNormal, permissiveMotifConfig(), identityResolver, labels, constMeanSim(0))

	require.NoError(t, err)
	require.Equal(t, 6, health.Candidates, "precondition: more candidates than the budget")
	require.Len(t, near, 2, "§5: hard cap 2 at normal")
	require.Empty(t, far, "normal is near lane only")
}

// The far lane is CLOSED below high effort, so a corpus whose motif groups are
// all far produces nothing at normal — however many candidates it has. Without
// this, "normal is near lane only" would be satisfied by a build that simply
// never produced a far group.
func TestBuildMotifBridges_NormalEmitsNothingForFarLaneGroups(t *testing.T) {
	seeds, clusters, labels := saturatedMotifCorpus(6)
	idx := &countingSearchIndex{} // no adjacency: every group is far

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortNormal, permissiveMotifConfig(), identityResolver, labels, constMeanSim(0.1))

	require.NoError(t, err)
	require.Equal(t, 6, health.Candidates, "precondition: the candidates exist")
	require.Empty(t, near)
	require.Empty(t, far, "the far lane opens only at high effort")
}

func TestBuildMotifBridges_HighOpensBothLanesWithinTheirBudgets(t *testing.T) {
	seeds, clusters, labels := saturatedMotifCorpus(20)
	idx := &countingSearchIndex{} // no SIMILAR_TO edges => every group is far

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, permissiveMotifConfig(), identityResolver, labels, constMeanSim(0.1))

	require.NoError(t, err)
	require.Equal(t, 20, health.Candidates)
	require.Empty(t, near, "no adjacency was offered, so nothing is in the near lane")
	nearBudget, farBudget := motifSubBudget(EffortHigh)
	require.Len(t, far, farBudget)
	require.LessOrEqual(t, len(near), nearBudget)
}

// A saturated far lane must not consume the near lane's slots, and vice versa
// (MN8). Asserted on a corpus that saturates BOTH.
func TestBuildMotifBridges_LanesCannotStarveEachOther(t *testing.T) {
	seeds, clusters, labels := saturatedMotifCorpus(20)
	// Give the first pair a SIMILAR_TO edge, so exactly one group is near.
	idx := &laneSplittingIndex{nearPair: [2]string{"kb/a00/fa00.md", "kb/b00/fb00.md"}}

	near, far, _, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, permissiveMotifConfig(), identityResolver, labels, constMeanSim(0.1))

	require.NoError(t, err)
	require.Len(t, near, 1, "the one adjacent group takes a near slot")
	nearBudget, farBudget := motifSubBudget(EffortHigh)
	require.Len(t, far, farBudget, "the far lane still fills its own budget in full")
	require.LessOrEqual(t, len(near), nearBudget)
}

// allAdjacentIndex reports every member set as mutually adjacent, so every
// group lands in the near lane.
type allAdjacentIndex struct {
	SearchQuery
}

func (a *allAdjacentIndex) TokenDF(context.Context, string, string, string) (int, error) {
	return 2, nil
}

func (a *allAdjacentIndex) ReverseDependentPaths(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (a *allAdjacentIndex) SimilarityAdjacency(_ context.Context, paths []string) (store.SimilarityGraph, error) {
	var pairs [][2]string
	for i := 0; i < len(paths); i++ {
		for j := i + 1; j < len(paths); j++ {
			pairs = append(pairs, [2]string{paths[i], paths[j]})
		}
	}
	return store.NewSimilarityGraph(pairs), nil
}

// laneSplittingIndex reports adjacency for exactly one pair.
type laneSplittingIndex struct {
	SearchQuery
	nearPair [2]string
}

func (l *laneSplittingIndex) TokenDF(context.Context, string, string, string) (int, error) {
	return 2, nil
}

func (l *laneSplittingIndex) ReverseDependentPaths(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (l *laneSplittingIndex) SimilarityAdjacency(_ context.Context, paths []string) (store.SimilarityGraph, error) {
	hits := 0
	for _, p := range paths {
		if p == l.nearPair[0] || p == l.nearPair[1] {
			hits++
		}
	}
	if hits == 2 {
		return store.NewSimilarityGraph([][2]string{l.nearPair}), nil
	}
	return store.NewSimilarityGraph(nil), nil
}

// permissiveMotifConfig keeps the quality gates out of the way so that budget
// tests measure budgets and nothing else.
func permissiveMotifConfig() QualityConfig {
	return QualityConfig{CohFloor: 0, QualityFloor: 0, WCoh: 1, WGap: 1, WSpec: 1, MaxMembers: 5}
}

// ── L6: a failed pass must not read like an empty one ─────────────────────

func TestMotifBridgeHealthLines_FailureIsNotSuccessShapedZeros(t *testing.T) {
	failed := motifBridgeHealthLines(motifEnumHealth{
		Failure: "similarity adjacency unavailable", Candidates: 3, Ceiling: 12}, 0, 0)
	require.Len(t, failed, 1)
	require.Contains(t, failed[0], "motif bridges unavailable this session")
	require.Contains(t, failed[0], "NOT a statement about the corpus")
	require.NotContains(t, failed[0], "0 near, 0 far",
		"a failed pass must not report the shape of an axis that looked and found nothing")

	// The complement: an axis that genuinely found nothing stays silent, and one
	// that found something reports it. Without these the assertion above is
	// satisfied by a function that always says "unavailable".
	require.Empty(t, motifBridgeHealthLines(motifEnumHealth{Ceiling: 12}, 0, 0))
	found := motifBridgeHealthLines(motifEnumHealth{Candidates: 2, Ceiling: 12}, 1, 1)
	require.Contains(t, found[0], "motif bridges: 2 candidates, 1 near, 1 far")
}

// ── L1: nested duplicate groups ───────────────────────────────────────────

// Every token-2 family strictly CONTAINS its key's verbatim group by
// construction, so the two compete for the same scarce slots and render the
// same `Bridge token:` line with nested member sets. The contained one is
// suppressed: an agent judging {A,B} and then {A,B,C} spends two of eight slots
// on one question.
func TestRankAndCap_SuppressesStrictlyContainedGroups(t *testing.T) {
	small := BridgeSeedSet{Token: "shared-shape", Q: 9, Members: []factForLLM{
		{File: "kb/a/1.md"}, {File: "kb/b/2.md"}}}
	big := BridgeSeedSet{Token: "shared-shape", Q: 1, Members: []factForLLM{
		{File: "kb/a/1.md"}, {File: "kb/b/2.md"}, {File: "kb/c/3.md"}}}

	got := rankAndCap([]BridgeSeedSet{small, big}, 8)

	require.Len(t, got, 1, "the contained group goes, whichever ranks higher")
	require.Len(t, got[0].Members, 3, "the SUPERSET survives — it carries strictly more evidence")
}

// Overlapping-but-not-contained groups both survive: they are different
// questions, and suppressing either would lose a bridge.
func TestRankAndCap_KeepsOverlappingGroupsThatAreNotContained(t *testing.T) {
	a := BridgeSeedSet{Token: "alpha-shape", Q: 2, Members: []factForLLM{
		{File: "kb/a/1.md"}, {File: "kb/b/2.md"}}}
	b := BridgeSeedSet{Token: "bravo-shape", Q: 1, Members: []factForLLM{
		{File: "kb/b/2.md"}, {File: "kb/c/3.md"}}}

	require.Len(t, rankAndCap([]BridgeSeedSet{a, b}, 8), 2)
}

// Suppression happens BEFORE the budget is spent, or it saves no slots.
func TestRankAndCap_SuppressionPrecedesTheBudget(t *testing.T) {
	contained := BridgeSeedSet{Token: "shared-shape", Q: 9, Members: []factForLLM{
		{File: "kb/a/1.md"}, {File: "kb/b/2.md"}}}
	superset := BridgeSeedSet{Token: "shared-shape", Q: 8, Members: []factForLLM{
		{File: "kb/a/1.md"}, {File: "kb/b/2.md"}, {File: "kb/c/3.md"}}}
	other := BridgeSeedSet{Token: "other-shape", Q: 1, Members: []factForLLM{
		{File: "kb/d/4.md"}, {File: "kb/e/5.md"}}}

	got := rankAndCap([]BridgeSeedSet{contained, superset, other}, 2)

	require.Len(t, got, 2)
	require.Equal(t, "shared-shape", got[0].Token)
	require.Len(t, got[0].Members, 3)
	require.Equal(t, "other-shape", got[1].Token,
		"the slot freed by suppression goes to a different question, not to the duplicate")
}
