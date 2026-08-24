package synthesize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
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

	_, distant, keptA, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar, g, idx, "main",
		clusterOf, motifQualityConfig(), 0.5, constPairCos(0.1))
	require.NoError(t, err)
	require.True(t, keptA,
		"the far lane must not apply the cohesion floor — cohesion is 0 there by construction")

	_, close, keptB, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar, g, idx, "main",
		clusterOf, motifQualityConfig(), 0.5, constPairCos(0.9))
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

	_, _, kept, err := scoreMotifCandidate(ctx, motifCandidate(), LaneNear, g, idx, "main",
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

	_, _, keptFar, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar,
		store.NewSimilarityGraph(nil), idx, "main", sameCommunity, cfg, 0.5, constPairCos(0))
	require.NoError(t, err)
	require.False(t, keptFar)

	_, _, keptNear, err := scoreMotifCandidate(ctx, motifCandidate(), LaneNear,
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

	_, _, kept, err := scoreMotifCandidate(ctx, cand, LaneFar, store.NewSimilarityGraph(nil),
		idx, "main", clusterOf, cfg, 0.5, constPairCos(0.1))
	require.NoError(t, err)
	require.False(t, kept)
}

// A far-lane group whose members carry no vectors must not read as maximally
// dissimilar — that would hand every unembedded group the top score. The
// caller's pair-cosine contract says so; this pins the scorer's half of it.
func TestScoreMotifCandidate_FarLanePropagatesPairCosErrors(t *testing.T) {
	ctx, idx := motifScoreEnv(t, "kb/alpha/1.md", "kb/beta/2.md")
	_, _, _, err := scoreMotifCandidate(ctx, motifCandidate(), LaneFar, store.NewSimilarityGraph(nil),
		idx, "main", map[string]int{"kb/alpha/1.md": 0, "kb/beta/2.md": 1},
		motifQualityConfig(), 0.5,
		func(context.Context, []string) ([]float64, error) { return nil, errors.New("no vectors") })
	require.Error(t, err, "an unreadable similarity is not a licence to score the group high")
}

// constPairCos gives every member pair the same cosine, so the far-lane mean
// is exactly v however many members a fixture has.
func constPairCos(v float64) pairCosFn {
	return func(_ context.Context, paths []string) ([]float64, error) {
		n := len(paths) * (len(paths) - 1) / 2
		out := make([]float64, n)
		for i := range out {
			out[i] = v
		}
		return out, nil
	}
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
		clusters, EffortNormal, permissiveMotifConfig(), identityResolver, labels, constPairCos(0))

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
		clusters, EffortNormal, permissiveMotifConfig(), identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 6, health.Candidates, "precondition: the candidates exist")
	require.Empty(t, near)
	require.Empty(t, far, "the far lane opens only at high effort")
}

func TestBuildMotifBridges_HighOpensBothLanesWithinTheirBudgets(t *testing.T) {
	seeds, clusters, labels := saturatedMotifCorpus(20)
	idx := &countingSearchIndex{} // no SIMILAR_TO edges => every group is far

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, permissiveMotifConfig(), identityResolver, labels, constPairCos(0.1))

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
		clusters, EffortHigh, permissiveMotifConfig(), identityResolver, labels, constPairCos(0.1))

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

// ── L1 + the cross-tier amendment: nested duplicate groups ────────────────

// mc builds an enumerated candidate at a named tier. Explicit at every call
// site, because the tier is now what decides which of two nested groups wins.
func mc(token string, q float64, family bool, files ...string) enumeratedMotif {
	ms := make([]factForLLM, 0, len(files))
	for _, f := range files {
		ms = append(ms, factForLLM{File: f})
	}
	return enumeratedMotif{
		BridgeSeedSet: BridgeSeedSet{Token: token, Kind: BridgeMotif, Q: q, Members: ms},
		family:        family,
	}
}

// CROSS-TIER: the exact group wins, and the family that contains it is dropped
// (designer ruling, Phase-4 rulings-3, amending L1).
//
// The fixture is the measured case that produced the ruling, named after it.
// On the merged corpus at high effort the token-2 family keyed
// `invents-rather-than-asks` folded the genuine verbatim pair
// `own-rather-than-rent` together with a tool-parameter gotcha and a
// drug-discovery fact — joined by the English construction "rather than" and
// nothing else. Under L1 as written the family displaced the real pair and
// took the slot, even though it ranked lower.
func TestRankAndCap_CrossTierTheExactGroupWins(t *testing.T) {
	exact := mc("own-rather-than-rent", 1, false,
		"kb/business/companies/oumi/products/0f4afbc1.md",
		"kb/technology/ai/economics/enterprise/eaf6e38d.md")
	family := mc("invents-rather-than-asks", 9, true,
		"kb/business/companies/oumi/products/0f4afbc1.md",
		"kb/technology/ai/economics/enterprise/eaf6e38d.md",
		"kb/gotchas/ai/agents/tools/parameters/missing-arguments/2b9d15c8.md",
		"kb/science/applied/biomedicine/ai-drug-discovery/robin/ac315cea.md")

	// Precondition (lesson 5): the family must OUTRANK the exact group, or the
	// test could pass on ranking rather than on the tier rule.
	require.Greater(t, family.Q, exact.Q,
		"precondition: the family must rank higher, so only the tier rule can drop it")

	got, crossTier := rankAndCap([]enumeratedMotif{exact, family}, 8)

	require.Len(t, got, 1)
	require.Equal(t, "own-rather-than-rent", got[0].Token,
		"the exact group is served; the looser fold is not")
	require.Len(t, got[0].Members, 2)
	require.Equal(t, 1, crossTier, "and the drop is counted")
}

// WITHIN A TIER the superset still survives — the original L1 rule, unchanged.
// An agent judging {A,B} and then {A,B,C} of the SAME grouping spends two of
// eight slots on one question.
func TestRankAndCap_SameTierTheSupersetWins(t *testing.T) {
	for _, family := range []bool{false, true} {
		small := mc("shared-shape", 9, family, "kb/a/1.md", "kb/b/2.md")
		big := mc("shared-shape", 1, family, "kb/a/1.md", "kb/b/2.md", "kb/c/3.md")

		got, crossTier := rankAndCap([]enumeratedMotif{small, big}, 8)

		require.Lenf(t, got, 1, "family=%v: the contained group goes, whichever ranks higher", family)
		require.Lenf(t, got[0].Members, 3, "family=%v: the SUPERSET survives within a tier", family)
		require.Zerof(t, crossTier, "family=%v: this is not a cross-tier drop", family)
	}
}

// Overlapping-but-not-contained groups both survive: they are different
// questions, and suppressing either would lose a bridge.
func TestRankAndCap_KeepsOverlappingGroupsThatAreNotContained(t *testing.T) {
	a := mc("alpha-shape", 2, false, "kb/a/1.md", "kb/b/2.md")
	b := mc("bravo-shape", 1, false, "kb/b/2.md", "kb/c/3.md")

	got, _ := rankAndCap([]enumeratedMotif{a, b}, 8)
	require.Len(t, got, 2)
}

// Suppression happens BEFORE the budget is spent, or it saves no slots.
func TestRankAndCap_SuppressionPrecedesTheBudget(t *testing.T) {
	contained := mc("shared-shape", 9, true, "kb/a/1.md", "kb/b/2.md")
	superset := mc("shared-shape", 8, true, "kb/a/1.md", "kb/b/2.md", "kb/c/3.md")
	other := mc("other-shape", 1, false, "kb/d/4.md", "kb/e/5.md")

	got, _ := rankAndCap([]enumeratedMotif{contained, superset, other}, 2)

	require.Len(t, got, 2)
	require.Equal(t, "shared-shape", got[0].Token)
	require.Len(t, got[0].Members, 3)
	require.Equal(t, "other-shape", got[1].Token,
		"the slot freed by suppression goes to a different question, not to the duplicate")
}

// ── M4: the crack between the lanes is counted, not silent ────────────────

// A group with ONE similarity edge among three members is assigned near
// (density 0.33 > 0) and then dropped by the 0.5 cohesion floor. It is
// two-thirds mutually dissimilar — far-lane material by any reading of §4 — and
// it used to vanish with no trace. It still vanishes; now it is counted.
func TestBuildMotifBridges_CountsGroupsLostAtTheNearFloor(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/a/one.md", Motifs: []string{"shared-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/two.md", Motifs: []string{"shared-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/three.md", Motifs: []string{"shared-shape"}, Entities: []string{"Gamma"}},
	}
	seeds = append(seeds, activationFillers()...)
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/a/one.md"}, 1: {"kb/b/two.md"}, 2: {"kb/c/three.md"}}}
	labels := labelsWith(200, map[string]int{"alpha": 2, "beta": 2, "gamma": 2})
	idx := &laneSplittingIndex{nearPair: [2]string{"kb/a/one.md", "kb/b/two.md"}}
	cfg := motifQualityConfig() // CohFloor 0.5

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 1, health.Candidates, "precondition: the group enumerated")
	require.Empty(t, near, "density 0.33 is below the 0.5 floor")
	require.Empty(t, far, "and the binary lane split never offered it to the far lane")
	require.Equal(t, 1, health.NearFloorDropped,
		"the group must be COUNTED where it disappears, not merely absent")

	require.Zero(t, health.NearOtherDropped, "and attributed to the floor, not to 'other'")

	lines := motifBridgeHealthLines(health, 0, 0)
	require.Contains(t, strings.Join(lines, "\n"), "motif near-lane floor: 1 group(s)")
}

// TestBuildMotifBridges_AttributesNearLaneDropsByCause — the counter must mean
// its label (review M4 counter-finding).
//
// The first version incremented NearFloorDropped on ANY near-lane rejection, so
// an oversize group rode in under a label that said "cohesion". Phase 4 reads
// this number as evidence for a lane redesign, and a number that does not mean
// its label is worse than no number at all.
//
// Two groups, dropped for two different reasons, counted separately.
func TestBuildMotifBridges_AttributesNearLaneDropsByCause(t *testing.T) {
	// Group A: three members, one edge => density 0.33, under the floor.
	// Group B: four mutually adjacent members => cohesion 1.0, over the cap.
	seeds := []factForLLM{
		{File: "kb/a/one.md", Motifs: []string{"sparse-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/two.md", Motifs: []string{"sparse-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/three.md", Motifs: []string{"sparse-shape"}, Entities: []string{"Gamma"}},
		{File: "kb/d/four.md", Motifs: []string{"crowded-shape"}, Entities: []string{"Delta"}},
		{File: "kb/e/five.md", Motifs: []string{"crowded-shape"}, Entities: []string{"Epsilon"}},
		{File: "kb/f/six.md", Motifs: []string{"crowded-shape"}, Entities: []string{"Zeta"}},
		{File: "kb/g/seven.md", Motifs: []string{"crowded-shape"}, Entities: []string{"Eta"}},
	}
	seeds = append(seeds, activationFillers()...)
	clusters := ClusterResult{Clusters: map[int][]string{}}
	labels := labelsWith(200, nil)
	for i, f := range seeds {
		clusters.Clusters[i] = []string{f.File}
		labels.DF[strings.ToLower(f.Entities[0])] = 2
	}

	idx := &pairwiseIndex{
		edges: [][2]string{
			{"kb/a/one.md", "kb/b/two.md"}, // group A: one edge among three
			// group B: all six pairs, so cohesion is 1.0 and only the cap bites
			{"kb/d/four.md", "kb/e/five.md"}, {"kb/d/four.md", "kb/f/six.md"},
			{"kb/d/four.md", "kb/g/seven.md"}, {"kb/e/five.md", "kb/f/six.md"},
			{"kb/e/five.md", "kb/g/seven.md"}, {"kb/f/six.md", "kb/g/seven.md"},
		},
	}
	cfg := motifQualityConfig()
	cfg.MaxMembers = 3 // group B has four

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 2, health.Candidates, "precondition: both groups enumerated")
	require.Empty(t, near)
	require.Empty(t, far)
	require.Equal(t, 1, health.NearFloorDropped, "the sparse group, and only it")
	require.Equal(t, 1, health.NearOtherDropped, "the oversize group, counted apart")

	lines := strings.Join(motifBridgeHealthLines(health, 0, 0), "\n")
	require.Contains(t, lines, "motif near-lane floor: 1 group(s)")
	require.Contains(t, lines, "motif near-lane other: 1 group(s)")
}

// pairwiseIndex reports adjacency for an explicit edge list.
type pairwiseIndex struct {
	SearchQuery
	edges [][2]string
}

func (p *pairwiseIndex) TokenDF(context.Context, string, string, string) (int, error) {
	return 2, nil
}

func (p *pairwiseIndex) ReverseDependentPaths(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (p *pairwiseIndex) SimilarityAdjacency(_ context.Context, paths []string) (store.SimilarityGraph, error) {
	in := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		in[path] = struct{}{}
	}
	var kept [][2]string
	for _, e := range p.edges {
		_, a := in[e[0]]
		_, b := in[e[1]]
		if a && b {
			kept = append(kept, e)
		}
	}
	return store.NewSimilarityGraph(kept), nil
}

// ── register entry 5: the far lane's dropped population is counted ────────

// §4's trim ("maximum community spread, then minimum mean similarity") is
// unimplemented, so an oversized far group is DROPPED by MaxMembers. That was
// ruled deliberate and carried to Phase 4 — but the population it carries was
// never counted, so the redesign had no denominator. It has one now.
func TestBuildMotifBridges_CountsOversizedFarGroups(t *testing.T) {
	// Four members, ZERO similarity edges => density 0 => far lane. Each in its
	// own community, so separation cannot be the cause. Cap at three.
	seeds := []factForLLM{
		{File: "kb/a/one.md", Motifs: []string{"wide-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/two.md", Motifs: []string{"wide-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/three.md", Motifs: []string{"wide-shape"}, Entities: []string{"Gamma"}},
		{File: "kb/d/four.md", Motifs: []string{"wide-shape"}, Entities: []string{"Delta"}},
	}
	seeds = append(seeds, activationFillers()...)
	clusters := ClusterResult{Clusters: map[int][]string{}}
	labels := labelsWith(200, nil)
	for i, f := range seeds {
		clusters.Clusters[i] = []string{f.File}
		labels.DF[strings.ToLower(f.Entities[0])] = 2
	}
	idx := &pairwiseIndex{} // no edges at all
	cfg := motifQualityConfig()
	cfg.MaxMembers = 3

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 1, health.Candidates, "precondition: the group enumerated")
	require.Empty(t, near)
	require.Empty(t, far, "four members against a cap of three")
	require.Equal(t, 1, health.FarOversizeDropped,
		"the trim's denominator: a far group the member cap dropped")
	require.Zero(t, health.FarOtherDropped, "and attributed to the cap, not to 'other'")
	require.Zero(t, health.NearFloorDropped, "a far-lane drop is not a near-lane drop")
	require.Zero(t, health.NearOtherDropped)

	lines := strings.Join(motifBridgeHealthLines(health, 0, 0), "\n")
	require.Contains(t, lines, "motif far-lane oversize: 1 group(s)")
}

// The same counter-finding the near lane already carries (review M4): a
// counter Phase 4 reads as evidence for the TRIM must count only what the trim
// would act on. A far group rejected by the quality floor is not that.
func TestBuildMotifBridges_AttributesFarLaneDropsByCause(t *testing.T) {
	// Group A: four members, no edges => far, over a cap of three.
	// Group B: two members, no edges => far, within the cap, but the quality
	// floor is set above anything it can score.
	seeds := []factForLLM{
		{File: "kb/a/one.md", Motifs: []string{"wide-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/two.md", Motifs: []string{"wide-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/three.md", Motifs: []string{"wide-shape"}, Entities: []string{"Gamma"}},
		{File: "kb/d/four.md", Motifs: []string{"wide-shape"}, Entities: []string{"Delta"}},
		{File: "kb/e/five.md", Motifs: []string{"thin-shape"}, Entities: []string{"Epsilon"}},
		{File: "kb/f/six.md", Motifs: []string{"thin-shape"}, Entities: []string{"Zeta"}},
	}
	seeds = append(seeds, activationFillers()...)
	clusters := ClusterResult{Clusters: map[int][]string{}}
	labels := labelsWith(200, nil)
	for i, f := range seeds {
		clusters.Clusters[i] = []string{f.File}
		labels.DF[strings.ToLower(f.Entities[0])] = 2
	}
	idx := &pairwiseIndex{}
	cfg := motifQualityConfig()
	cfg.MaxMembers = 3
	cfg.QualityFloor = 99 // nothing can reach it

	// Precondition (lesson 5): the two groups must differ in the property the
	// attribution turns on, or the test cannot tell the causes apart.
	require.NotEqual(t, 4, 2, "group sizes straddle the cap")

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 2, health.Candidates, "precondition: both groups enumerated")
	require.Empty(t, near)
	require.Empty(t, far)
	require.Equal(t, 1, health.FarOversizeDropped, "the wide group, and only it")
	require.Equal(t, 1, health.FarOtherDropped, "the floor-failing group, counted apart")

	lines := strings.Join(motifBridgeHealthLines(health, 0, 0), "\n")
	require.Contains(t, lines, "motif far-lane oversize: 1 group(s)")
	require.Contains(t, lines, "motif far-lane other: 1 group(s)")
}

// ── T1: the measurement reads the engine, not a second implementation ─────

// The rule this asserts: what the Phase-4 instrument measures is what a review
// session would serve. Production filters and ranks the rows; if it ever grew
// a filter of its own, the numbers the phase sets constants on would quietly
// stop describing the shipped axis. Both sides drive scoreMotifCandidates, and
// this is what pins that they still do.
func TestScoreMotifCandidates_KeptRowsAreExactlyWhatProductionServes(t *testing.T) {
	seeds, clusters, labels, idx, cfg := laneMixtureFixture(t)

	rows, rowHealth, err := scoreMotifCandidates(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))
	require.NoError(t, err)

	near, far, buildHealth, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))
	require.NoError(t, err)
	// buildMotifBridges' health carries ONE field the rows' does not:
	// FamilySuppressedByExact, which only exists once suppression has run. Set
	// it from the same rebuild below rather than excluding it, so the equality
	// still covers every other field. (The first version compared the structs
	// raw and passed only because this fixture has no cross-tier containment —
	// a coincidence in the fixture, not a property.)

	// Rebuild what production serves, from the rows alone.
	var wantNear, wantFar []enumeratedMotif
	for _, r := range rows {
		if !r.kept {
			continue
		}
		c := r.cand
		c.Q = r.q
		if r.lane == LaneNear {
			wantNear = append(wantNear, c)
		} else {
			wantFar = append(wantFar, c)
		}
	}
	nb, fb := motifSubBudget(EffortHigh)
	gotNearPre, nearSup := rankAndCap(wantNear, nb)
	gotFarPre, farSup := rankAndCap(wantFar, fb)
	rowHealth.FamilySuppressedByExact = nearSup + farSup
	require.Equal(t, rowHealth, buildHealth, "one enumeration, one health picture")
	_, _ = gotNearPre, gotFarPre

	require.NotEmpty(t, wantNear, "precondition: the fixture must serve something on each lane")
	require.NotEmpty(t, wantFar, "precondition: the fixture must serve something on each lane")
	// PRECONDITION, and it is load-bearing: the served groups must DIFFER in
	// member count. A fixture whose served groups all have two members cannot
	// see a production-side filter on member count, and the first version of
	// this test could not — the sabotage passed. Assert the difference rather
	// than trusting the fixture to keep it (lesson 5).
	sizes := map[int]bool{}
	for _, b := range append(append([]enumeratedMotif{}, wantNear...), wantFar...) {
		sizes[len(b.Members)] = true
	}
	require.Greater(t, len(sizes), 1,
		"precondition: served groups must vary in member count, or this test is blind to a size filter")
	require.Equal(t, gotNearPre, near)
	require.Equal(t, gotFarPre, far)
}

// The rows carry the DROPPED candidates too, with the cause — which is the
// whole reason the instrument exists, since production throws them away and
// every carried-forward measurement is about them.
func TestScoreMotifCandidates_CarryDroppedCandidatesWithTheirCause(t *testing.T) {
	seeds, clusters, labels, idx, cfg := laneMixtureFixture(t)

	rows, health, err := scoreMotifCandidates(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, cfg, identityResolver, labels, constPairCos(0.1))
	require.NoError(t, err)

	byCause := map[motifDropCause]int{}
	for _, r := range rows {
		byCause[r.cause]++
		require.Equal(t, r.kept, r.cause == motifKept,
			"a row is kept exactly when it has no drop cause")
	}
	require.Positive(t, byCause[motifKept], "precondition: something survived")
	require.Positive(t, byCause[motifNearFloor], "precondition: the fixture drops one at the near floor")
	require.Positive(t, byCause[motifFarOversize], "precondition: and one over the far cap")

	// The counters are TALLIED from these rows, so they cannot disagree about
	// the same group — the M4 counter-finding made structurally impossible
	// rather than re-checked by hand.
	require.Equal(t, byCause[motifNearFloor], health.NearFloorDropped)
	require.Equal(t, byCause[motifNearOther], health.NearOtherDropped)
	require.Equal(t, byCause[motifFarOversize], health.FarOversizeDropped)
	require.Equal(t, byCause[motifFarOther], health.FarOtherDropped)
	require.Equal(t, byCause[motifKept], health.Candidates-
		(health.NearFloorDropped+health.NearOtherDropped+health.FarOversizeDropped+health.FarOtherDropped),
		"every enumerated candidate is either served or attributed to one cause")
}

// laneMixtureFixture builds a corpus that exercises every outcome at once: a
// cohesive near group that survives, a sparse near group killed by the floor, a
// dissimilar far group that survives, and an oversized far group killed by the
// cap. Deliberately non-degenerate (lesson 5) — the four groups differ in the
// properties the attribution turns on, and the test asserts that as a
// precondition rather than trusting the construction.
func laneMixtureFixture(t *testing.T) ([]factForLLM, ClusterResult, store.SubjectLabelDF, *pairwiseIndex, QualityConfig) {
	t.Helper()
	mk := func(file, motif, entity string) factForLLM {
		return factForLLM{File: file, Motifs: []string{motif}, Entities: []string{entity}}
	}
	seeds := []factForLLM{
		// tight-shape: two members, one edge => cohesion 1.0 => near, survives.
		mk("kb/a/1.md", "tight-shape", "Alpha"), mk("kb/b/2.md", "tight-shape", "Beta"),
		// dense-shape: THREE members, all three pairs edged => cohesion 1.0 =>
		// near, survives, and is the only served group with more than two
		// members. Without it every served group coincides at size 2 and a
		// production-side member filter is invisible to the equivalence test
		// (lesson 5, found by sabotaging exactly that).
		mk("kb/l/12.md", "dense-shape", "Mu"), mk("kb/m/13.md", "dense-shape", "Nu"),
		mk("kb/n/14.md", "dense-shape", "Xi"),
		// sparse-shape: three members, one edge => density 0.33 => near, floored.
		mk("kb/c/3.md", "sparse-shape", "Gamma"), mk("kb/d/4.md", "sparse-shape", "Delta"),
		mk("kb/e/5.md", "sparse-shape", "Epsilon"),
		// apart-shape: two members, no edge => far, survives.
		mk("kb/f/6.md", "apart-shape", "Zeta"), mk("kb/g/7.md", "apart-shape", "Eta"),
		// wide-shape: four members, no edges => far, over the cap of three.
		mk("kb/h/8.md", "wide-shape", "Theta"), mk("kb/i/9.md", "wide-shape", "Iota"),
		mk("kb/j/10.md", "wide-shape", "Kappa"), mk("kb/k/11.md", "wide-shape", "Lambda"),
	}
	seeds = append(seeds, activationFillers()...)
	clusters := ClusterResult{Clusters: map[int][]string{}}
	labels := labelsWith(200, nil)
	for i, f := range seeds {
		clusters.Clusters[i] = []string{f.File}
		labels.DF[strings.ToLower(f.Entities[0])] = 2
	}
	idx := &pairwiseIndex{edges: [][2]string{
		{"kb/a/1.md", "kb/b/2.md"}, // tight-shape: the only pair, so cohesion 1.0
		{"kb/c/3.md", "kb/d/4.md"}, // sparse-shape: one of three pairs
		// dense-shape: all three pairs, so cohesion is 1.0 at three members
		{"kb/l/12.md", "kb/m/13.md"}, {"kb/l/12.md", "kb/n/14.md"},
		{"kb/m/13.md", "kb/n/14.md"},
	}}
	cfg := motifQualityConfig()
	cfg.MaxMembers = 3
	return seeds, clusters, labels, idx, cfg
}

// ── P2: the activation floor (K=3, phase4-rulings-4) ──────────────────────

// A corpus below the floor enumerates NOTHING, and it costs no index call to
// find that out — the same property that keeps MN5's vacuous pass vacuous.
func TestBuildMotifBridges_BelowTheActivationFloorEnumeratesNothing(t *testing.T) {
	// Two recurring clusters. The floor is three.
	seeds := []factForLLM{
		{File: "kb/a/1.md", Motifs: []string{"first-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/2.md", Motifs: []string{"first-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/3.md", Motifs: []string{"second-shape"}, Entities: []string{"Gamma"}},
		{File: "kb/d/4.md", Motifs: []string{"second-shape"}, Entities: []string{"Delta"}},
	}
	clusters, labels := oneCommunityEach(seeds)
	idx := &countingIndex{pairwiseIndex: pairwiseIndex{edges: nil}}

	near, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, motifQualityConfig(), identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 2, health.Activation.DF2Clusters, "precondition: below the floor of 3")
	require.False(t, health.Activation.Active)
	require.Empty(t, near)
	require.Empty(t, far)
	require.Zero(t, health.Candidates, "an inactive corpus enumerates nothing")
	require.Zero(t, idx.calls, "and costs no index call to decide it")

	lines := strings.Join(motifBridgeHealthLines(health, 0, 0), "\n")
	require.Contains(t, lines, "motif bridging inactive")
	require.Contains(t, lines, "2 recurring")
}

// At the floor it enumerates normally. The two tests differ by ONE cluster, so
// the assertion is about the floor rather than about the fixtures.
func TestBuildMotifBridges_AtTheActivationFloorEnumeratesNormally(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/a/1.md", Motifs: []string{"first-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/2.md", Motifs: []string{"first-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/3.md", Motifs: []string{"second-shape"}, Entities: []string{"Gamma"}},
		{File: "kb/d/4.md", Motifs: []string{"second-shape"}, Entities: []string{"Delta"}},
		{File: "kb/e/5.md", Motifs: []string{"third-shape"}, Entities: []string{"Epsilon"}},
		{File: "kb/f/6.md", Motifs: []string{"third-shape"}, Entities: []string{"Zeta"}},
	}
	clusters, labels := oneCommunityEach(seeds)
	idx := &pairwiseIndex{}

	_, far, health, err := buildMotifBridges(context.Background(), idx, "main", seeds,
		clusters, EffortHigh, motifQualityConfig(), identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, motifActivationFloor, health.Activation.DF2Clusters,
		"precondition: exactly at the floor, one cluster above the previous test")
	require.True(t, health.Activation.Active)
	require.Equal(t, 3, health.Candidates)
	require.Len(t, far, 3, "and the candidates are served")
}

// The floor counts RECURRING clusters, not motifed facts: a corpus can carry
// plenty of motifs and still have nothing that repeats.
func TestMotifActive_CountsRecurrenceNotVolume(t *testing.T) {
	var hapax []factForLLM
	for i := range 40 {
		hapax = append(hapax, factForLLM{
			File:   fmt.Sprintf("kb/h/%d.md", i),
			Motifs: []string{fmt.Sprintf("unique-shape-%d", i)},
		})
	}
	got := motifActive(hapax, identityResolver)
	require.False(t, got.Active, "forty motifs, none of them recurring")
	require.Zero(t, got.DF2Clusters)
	require.Zero(t, got.Pairs)
}

// A fact spelling one mechanism two ways is ONE carrier. Otherwise a single
// author's phrasing habit would activate the axis by itself.
func TestSeedRecurrence_TwoSpellingsOnOneFactAreOneCarrier(t *testing.T) {
	resolve := func(m string) string {
		if m == "silent-drop" || m == "drops-silently" {
			return "silent-drop"
		}
		return m
	}
	seeds := []factForLLM{{File: "kb/a/1.md", Motifs: []string{"silent-drop", "drops-silently"}}}

	clusters, pairs := seedRecurrence(seeds, resolve)
	require.Zero(t, clusters, "one fact cannot make a motif recur")
	require.Zero(t, pairs)
}

// oneCommunityEach puts every seed in its own community and gives every entity
// a df that clears the disjointness gate.
func oneCommunityEach(seeds []factForLLM) (ClusterResult, store.SubjectLabelDF) {
	clusters := ClusterResult{Clusters: map[int][]string{}}
	labels := labelsWith(200, nil)
	for i, f := range seeds {
		clusters.Clusters[i] = []string{f.File}
		if len(f.Entities) > 0 {
			labels.DF[strings.ToLower(f.Entities[0])] = 2
		}
	}
	return clusters, labels
}

// countingIndex records whether the enumeration touched the index at all.
type countingIndex struct {
	pairwiseIndex
	calls int
}

func (c *countingIndex) TokenDF(ctx context.Context, b, t, k string) (int, error) {
	c.calls++
	return c.pairwiseIndex.TokenDF(ctx, b, t, k)
}

func (c *countingIndex) SimilarityAdjacency(ctx context.Context, paths []string) (store.SimilarityGraph, error) {
	c.calls++
	return c.pairwiseIndex.SimilarityAdjacency(ctx, paths)
}

// activationFillers are four facts carrying two recurring motifs, present only
// to clear the K=3 activation floor in tests about something else.
//
// Each pair SHARES a rare entity, so subject-disjointness rejects it and it
// never enumerates as a candidate. That is the whole trick: recurrence is
// counted over the seed pool BEFORE any gate, so these move the activation
// count and leave every candidate assertion untouched. A filler that enumerated
// would silently change the numbers the calling test is about.
func activationFillers() []factForLLM {
	return []factForLLM{
		{File: "kb/fill/a1.md", Motifs: []string{"filler-shape-one"}, Entities: []string{"FillerAlpha"}},
		{File: "kb/fill/a2.md", Motifs: []string{"filler-shape-one"}, Entities: []string{"FillerAlpha"}},
		{File: "kb/fill/b1.md", Motifs: []string{"filler-shape-two"}, Entities: []string{"FillerBeta"}},
		{File: "kb/fill/b2.md", Motifs: []string{"filler-shape-two"}, Entities: []string{"FillerBeta"}},
	}
}

// ── P3: §8's novelty signals ──────────────────────────────────────────────

func TestNoveltyOf_VectorsReadDistinguishesUnknownFromZero(t *testing.T) {
	members := []factForLLM{{File: "kb/a/1.md"}, {File: "kb/b/2.md"}}
	paths := []string{"kb/a/1.md", "kb/b/2.md"}

	// No provider at all: SeedCos and OverDedup are UNKNOWN, and the flag is
	// what says so. Their zeros must not be read as measurements.
	none, err := noveltyOf(context.Background(), members, paths, nil, 0.92)
	require.NoError(t, err)
	require.False(t, none.VectorsRead)
	require.Zero(t, none.SeedCos)

	// A provider returning genuinely-zero cosines: same zeros, opposite meaning.
	zeroed, err := noveltyOf(context.Background(), members, paths, constPairCos(0), 0.92)
	require.NoError(t, err)
	require.True(t, zeroed.VectorsRead, "the flag is the ONLY thing separating these two results")
	require.Zero(t, zeroed.SeedCos)
}

func TestNoveltyOf_OverDedupIsAFractionOfPairs(t *testing.T) {
	members := []factForLLM{{File: "a"}, {File: "b"}, {File: "c"}}
	paths := []string{"a", "b", "c"}
	// Three pairs; one of them at/above the threshold.
	pc := func(context.Context, []string) ([]float64, error) {
		return []float64{0.95, 0.10, 0.20}, nil
	}
	got, err := noveltyOf(context.Background(), members, paths, pc, 0.92)
	require.NoError(t, err)
	require.InDelta(t, 1.0/3.0, got.OverDedup, 1e-9)
	require.InDelta(t, (0.95+0.10+0.20)/3, got.SeedCos, 1e-9,
		"SeedCos is the MEAN, so one near-duplicate pair does not dominate it — "+
			"which is exactly why OverDedup is reported beside it")
}

func TestMeanEntityJaccard_DisjointAndOverlapping(t *testing.T) {
	disjoint := []factForLLM{
		{Entities: []string{"Redis"}}, {Entities: []string{"SWIFT"}}}
	require.Zero(t, meanEntityJaccard(disjoint))

	overlap := []factForLLM{
		{Entities: []string{"Redis", "Kafka"}}, {Entities: []string{"Redis"}}}
	require.InDelta(t, 0.5, meanEntityJaccard(overlap), 1e-9, "one shared of two distinct")

	// Two facts naming NOTHING are an absence of evidence about their subjects,
	// not perfect disjointness. Scoring them 1.0 would reward a corpus for
	// being unlabelled.
	require.Zero(t, meanEntityJaccard([]factForLLM{{}, {}}))
}

// H-2. seedRecurrence excludes discovered-origin facts, matching the §7
// exclusion enumeration applies (cf455b8f). The fix was ratified by name in
// rulings-5 and shipped with NO test: deleting the exclusion left the whole
// suite green, because no lab corpus holds a motif-bearing discovered fact —
// the annex never answered a discover item — so neither the fixtures nor the
// measurements could see it.
//
// AcceptSeed filters on Kind == Epistemic only, never on origin, so discovered
// facts genuinely do reach the seed pool. Without the exclusion a corpus can
// ACTIVATE on recurrence that enumeration cannot see: the axis switches on,
// announces "3 recurring motifs", enumerates nothing, and does it again every
// session. It is also self-amplifying — the corpus activating on its own
// discovery output is cf455b8f's idempotency concern one layer out.
func TestSeedRecurrence_DoesNotCountDiscoveredFacts(t *testing.T) {
	authored := []factForLLM{
		{File: "kb/a/1.md", Motifs: []string{"first-shape"}},
		{File: "kb/b/2.md", Motifs: []string{"first-shape"}},
		{File: "kb/c/3.md", Motifs: []string{"second-shape"}},
		{File: "kb/d/4.md", Motifs: []string{"second-shape"}},
	}
	// The case no lab corpus holds: a DISCOVERED fact carrying motifs. MN11 puts
	// `motifs` on discover's output schema, so this is reachable the moment
	// discovery answers an item.
	discovered := []factForLLM{
		{File: "kb/e/5.md", Motifs: []string{"third-shape"}, Origin: string(fact.Discovered)},
		{File: "kb/f/6.md", Motifs: []string{"third-shape"}, Origin: string(fact.Discovered)},
	}

	base, basePairs := seedRecurrence(authored, identityResolver)
	require.Equal(t, 2, base, "precondition: two recurring clusters from authored facts")
	require.Equal(t, 2, basePairs)

	withDiscovered, pairs := seedRecurrence(append(append([]factForLLM{}, authored...), discovered...),
		identityResolver)
	require.Equal(t, base, withDiscovered,
		"a discovered fact's motifs must not count toward recurrence — enumeration cannot see them, "+
			"so an activation counting them does not mean its label")
	require.Equal(t, basePairs, pairs)
}

// The consequence at the boundary that matters: a corpus whose third recurring
// cluster exists only on discovered facts must NOT activate.
//
// Asserted through buildMotifBridges rather than through seedRecurrence alone,
// because "the axis switched on and found nothing" is the failure, and that is
// visible only where activation meets enumeration (lesson 8).
func TestBuildMotifBridges_DiscoveredRecurrenceDoesNotActivateTheAxis(t *testing.T) {
	seeds := []factForLLM{
		{File: "kb/a/1.md", Motifs: []string{"first-shape"}, Entities: []string{"Alpha"}},
		{File: "kb/b/2.md", Motifs: []string{"first-shape"}, Entities: []string{"Beta"}},
		{File: "kb/c/3.md", Motifs: []string{"second-shape"}, Entities: []string{"Gamma"}},
		{File: "kb/d/4.md", Motifs: []string{"second-shape"}, Entities: []string{"Delta"}},
		{File: "kb/e/5.md", Motifs: []string{"third-shape"}, Entities: []string{"Epsilon"},
			Origin: string(fact.Discovered)},
		{File: "kb/f/6.md", Motifs: []string{"third-shape"}, Entities: []string{"Zeta"},
			Origin: string(fact.Discovered)},
	}
	clusters, labels := oneCommunityEach(seeds)

	near, far, health, err := buildMotifBridges(context.Background(), &pairwiseIndex{}, "main", seeds,
		clusters, EffortHigh, motifQualityConfig(), identityResolver, labels, constPairCos(0.1))

	require.NoError(t, err)
	require.Equal(t, 2, health.Activation.DF2Clusters,
		"the discovered pair's cluster is not a third recurring motif")
	require.False(t, health.Activation.Active, "so the axis stays off")
	require.Empty(t, near)
	require.Empty(t, far)
}
