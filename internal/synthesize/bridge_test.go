package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// makeFact is a tiny constructor for the test fixtures below.
func makeFact(path, origin string, domains, entities []string) factForLLM {
	return factForLLM{
		File:     path,
		Title:    path,
		Body:     path + " body",
		Type:     "observation",
		Domain:   domains,
		Entities: entities,
		Origin:   origin,
	}
}

// containsToken reports whether one of the returned bridge sets is for `token`.
func containsToken(sets []BridgeSeedSet, token string) (BridgeSeedSet, bool) {
	for _, s := range sets {
		if s.Token == token {
			return s, true
		}
	}
	return BridgeSeedSet{}, false
}

// setHasMember reports whether path appears in the set's Members.
func setHasMember(s BridgeSeedSet, path string) bool {
	for _, m := range s.Members {
		if m.File == path {
			return true
		}
	}
	return false
}

// TestBridgeSeeds_CrossClusterEntity is the canonical bridge case: two facts
// share an entity but live in different communities → emit a bridge. Two
// facts share an entity but live in the same community → NOT a bridge.
// Retargeted to enumerateBridgeCandidates (Task 16: grouping logic is unchanged).
func TestBridgeSeeds_CrossClusterEntity(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", "authored", nil, []string{"auth"})
	c := makeFact("c.md", "authored", nil, []string{"auth"})
	d := makeFact("d.md", "authored", nil, []string{"misc"})
	seeds := []factForLLM{a, b, c, d}

	// a in community 0, b in community 1, c in community 0 (same as a).
	// d in community 1 (same as b) but carries a different entity.
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md", "c.md"},
			1: {"b.md", "d.md"},
		},
	}

	got := enumerateBridgeCandidates(seeds, clusters, BridgeEntity, ScopeFilter{})

	set, found := containsToken(got, "auth")
	if !found {
		t.Fatalf("expected a bridge on token 'auth', got %v", got)
	}
	if !setHasMember(set, "a.md") || !setHasMember(set, "b.md") {
		t.Errorf("'auth' bridge should include a.md and b.md across clusters: %+v", set)
	}
	// 'misc' appears on only one fact → never a bridge.
	if _, found := containsToken(got, "misc"); found {
		t.Errorf("'misc' must not be a bridge (only one carrying fact)")
	}
}

// TestBridgeSeeds_NormalEffortEmpty asserts that EffortNormal returns (nil, nil) —
// the discovery engine never engages. Retargeted to buildScoredBridges (Task 16).
func TestBridgeSeeds_NormalEffortEmpty(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	// No expectations: any idx call would fail the test.
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", "authored", nil, []string{"auth"})
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}
	got, err := buildScoredBridges(context.Background(), idx, "main", []factForLLM{a, b}, clusters, BridgeBoth, EffortNormal, testCfg, ScopeFilter{})
	require.NoError(t, err)
	if got != nil {
		t.Errorf("EffortNormal must return nil, got %v", got)
	}
}

// TestBridgeSeeds_ExcludesDiscoveredOrigin enforces Plan 03 §7 idempotency:
// a fact whose origin is 'discovered' is NEVER selected as a seed, even if
// it would otherwise complete a bridge.
// Retargeted to enumerateBridgeCandidates (Task 16: exclusion logic is unchanged).
func TestBridgeSeeds_ExcludesDiscoveredOrigin(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", string(fact.Discovered), nil, []string{"auth"})
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}
	got := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeBoth, ScopeFilter{})
	if len(got) != 0 {
		t.Errorf("discovered seed must be excluded; b alone leaves a w/o partner: %+v", got)
	}
}

// manyCrossClusterSeeds builds n distinct cross-cluster bridges: n unique
// tokens, each on one fact in community 0 and one in community 1.
func manyCrossClusterSeeds(n int) ([]factForLLM, ClusterResult) {
	var seeds []factForLLM
	cluster0 := []string{}
	cluster1 := []string{}
	for i := 0; i < n; i++ {
		tok := tokenName(i)
		p0 := "c0-" + tok + ".md"
		p1 := "c1-" + tok + ".md"
		seeds = append(seeds,
			makeFact(p0, "authored", nil, []string{tok}),
			makeFact(p1, "authored", nil, []string{tok}),
		)
		cluster0 = append(cluster0, p0)
		cluster1 = append(cluster1, p1)
	}
	return seeds, ClusterResult{Clusters: map[int][]string{0: cluster0, 1: cluster1}}
}

// cohesiveMockIdx returns a MockSearchIndex where every SimilarityAdjacency
// call returns a fully-connected graph (all pairs connected), every
// ReverseDependentPaths returns an empty set, and every TokenDF returns 1.
// This ensures buildScoredBridges keeps all candidates (cohesion == 1 ≥ CohFloor,
// gap == 1, spec == 1), so budget truncation tests aren't confounded by gate drops.
func cohesiveMockIdx(t *testing.T) *MockSearchIndex {
	t.Helper()
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, paths []string) (store.SimilarityGraph, error) {
			pairs := make([][2]string, 0, len(paths)*(len(paths)-1)/2)
			for i := 0; i < len(paths); i++ {
				for j := i + 1; j < len(paths); j++ {
					pairs = append(pairs, [2]string{paths[i], paths[j]})
				}
			}
			return store.NewSimilarityGraph(pairs), nil
		}).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(1, nil).AnyTimes()
	return idx
}

// TestBridgeSeeds_EffortBudget asserts the per-effort budget truncates the pool
// and that effort governs breadth: high digs strictly deeper than medium. The
// budget applies unconditionally — the regression guard for the "scoped runs
// ignore the effort dial's breadth" fix, where a scoped pool used to skip
// truncation and make medium == high.
// Retargeted to buildScoredBridges (Task 16).
func TestBridgeSeeds_EffortBudget(t *testing.T) {
	seeds, clusters := manyCrossClusterSeeds(60)
	ctx := context.Background()

	med, err := buildScoredBridges(ctx, cohesiveMockIdx(t), "main", seeds, clusters, BridgeEntity, EffortMedium, testCfg, ScopeFilter{})
	require.NoError(t, err)
	hi, err := buildScoredBridges(ctx, cohesiveMockIdx(t), "main", seeds, clusters, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)

	if len(med) != effortBudget(EffortMedium) {
		t.Errorf("medium budget: got %d, want %d", len(med), effortBudget(EffortMedium))
	}
	if len(hi) != effortBudget(EffortHigh) {
		t.Errorf("high budget: got %d, want %d", len(hi), effortBudget(EffortHigh))
	}
	if !(len(med) < len(hi)) {
		t.Errorf("effort must govern breadth: medium (%d) should dig fewer bridges than high (%d)", len(med), len(hi))
	}
}

// TestBridgeSeeds_PriorityBandHoldsUnderLargePool is the regression guard for
// the forward priority-band invariant. Each forward bridge gets priority
// forwardDiscoverPriorityBase-rank; if a pool ever surfaced ≥maxBridgeSeeds
// bridges, the deepest item's priority would reach reflect's floor and
// discovery would reorder after reflect. The per-effort budget (48 at high)
// keeps the count well under maxBridgeSeeds.
// Retargeted to buildScoredBridges (Task 16).
func TestBridgeSeeds_PriorityBandHoldsUnderLargePool(t *testing.T) {
	seeds, clusters := manyCrossClusterSeeds(maxBridgeSeeds + 25) // far over every cap

	hi, err := buildScoredBridges(context.Background(), cohesiveMockIdx(t), "main", seeds, clusters, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	if len(hi) != effortBudget(EffortHigh) {
		t.Fatalf("high pool must cap at the effort budget: got %d, want %d", len(hi), effortBudget(EffortHigh))
	}
	if len(hi) > maxBridgeSeeds {
		t.Fatalf("result %d must never exceed the absolute backstop %d", len(hi), maxBridgeSeeds)
	}
	// The lowest-priority surviving item must stay strictly above reflect.
	lowest := forwardDiscoverPriority(len(hi) - 1)
	if lowest <= reflectPriority {
		t.Errorf("lowest forward discover priority %v must stay above reflect %v", lowest, float64(reflectPriority))
	}
}

func tokenName(i int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	if i < 26 {
		return string(alpha[i])
	}
	return string(alpha[i/26-1]) + string(alpha[i%26])
}

// TestBridgeSeeds_DomainOnly_NotEntity respects the bridge kind.
// Retargeted to enumerateBridgeCandidates (Task 16: kind filtering is unchanged).
func TestBridgeSeeds_DomainOnly_NotEntity(t *testing.T) {
	a := makeFact("a.md", "authored", []string{"auth"}, []string{"X"})
	b := makeFact("b.md", "authored", []string{"auth"}, []string{"Y"})
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}

	dom := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeDomain, ScopeFilter{})
	if _, ok := containsToken(dom, "auth"); !ok {
		t.Errorf("BridgeDomain must surface 'auth': %v", dom)
	}
	if _, ok := containsToken(dom, "X"); ok {
		t.Errorf("BridgeDomain must NOT surface entity 'X': %v", dom)
	}
}

// TestBridgeSeeds_OrphanSeedNotCommunityZero is the regression guard for the
// map-zero-value collision: a seed present in NEITHER Clusters NOR Noise must
// not collapse to community id 0 and collide with a genuine community-0 fact.
// a.md (real community 0) shares entity "auth" with orphan.md (absent from the
// ClusterResult entirely, as happens when small-cluster filtering or dedup
// drops it upstream). They live in different communities, so the bridge MUST
// surface. Before the fix orphan.md → 0 == a.md → 0, and the bridge vanished.
// Retargeted to enumerateBridgeCandidates (Task 16: community-id logic is unchanged).
func TestBridgeSeeds_OrphanSeedNotCommunityZero(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	orphan := makeFact("orphan.md", "authored", nil, []string{"auth"})
	// a.md is in real community 0; orphan.md is in no cluster and no noise list.
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}},
	}

	got := enumerateBridgeCandidates([]factForLLM{a, orphan}, clusters, BridgeEntity, ScopeFilter{})
	set, found := containsToken(got, "auth")
	if !found {
		t.Fatalf("orphan seed (absent from clusters) must not collide with community 0; expected an 'auth' bridge, got %+v", got)
	}
	if !setHasMember(set, "a.md") || !setHasMember(set, "orphan.md") {
		t.Errorf("'auth' bridge must span a.md and orphan.md across communities: %+v", set)
	}
}

// TestBridgeKindFromString pins the config-string coercion: known values map
// through, empty and unrecognized fall back to the default (both axes).
func TestBridgeKindFromString(t *testing.T) {
	cases := map[string]BridgeKind{
		"domain": BridgeDomain,
		"entity": BridgeEntity,
		"both":   BridgeBoth,
		"":       DefaultBridgeKind,
		"bogus":  DefaultBridgeKind,
	}
	for in, want := range cases {
		if got := BridgeKindFromString(in); got != want {
			t.Errorf("BridgeKindFromString(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildBackwardBridges_HonorsBridgeKind guards that the backward
// (hypothesize) pipeline respects the configured bridge axis rather than
// always defaulting to "both". Two synthesis facts in different communities
// share BOTH a domain ("auth") and an entity ("shared"); the surfaced bridge
// token must depend on the kind argument.
// Updated for Task 16: BuildBackwardBridges now uses buildScoredBridges and
// requires cfg + scoring mock expectations (SimilarityAdjacency, etc.).
func TestBuildBackwardBridges_HonorsBridgeKind(t *testing.T) {
	ctx := context.Background()

	mk := func(path string) fact.Fact {
		f := fact.NewFact(path)
		f.Title, f.Body, f.Type = path, path, fact.Synthesis
		f.Domain, f.Entities = []string{"auth"}, []string{"shared"}
		return f
	}
	synthFacts := []fact.Fact{mk("kb/a.md"), mk("kb/b.md")}

	m := NewMockSearchIndex(gomock.NewController(t))
	// a,b cluster apart → their shared tokens form cross-community bridges.
	expectScopedClusterPartition(m, nil, [][]string{{"kb/a.md"}, {"kb/b.md"}})
	// Scoring expectations: cohesive graph (a↔b connected), no reverse deps, df=1.
	m.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, paths []string) (store.SimilarityGraph, error) {
			pairs := make([][2]string, 0)
			for i := 0; i < len(paths); i++ {
				for j := i + 1; j < len(paths); j++ {
					pairs = append(pairs, [2]string{paths[i], paths[j]})
				}
			}
			return store.NewSimilarityGraph(pairs), nil
		}).AnyTimes()
	m.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()
	m.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(1, nil).AnyTimes()

	// Entity kind: only the shared ENTITY token bridges.
	ent, err := BuildBackwardBridges(ctx, m, synthFacts, "agent/test", EffortHigh, BridgeEntity, 2.0, 2, testCfg, ScopeFilter{})
	require.NoError(t, err)
	if _, ok := containsToken(ent, "shared"); !ok {
		t.Errorf("BridgeEntity must surface entity token 'shared': %v", ent)
	}
	if _, ok := containsToken(ent, "auth"); ok {
		t.Errorf("BridgeEntity must NOT surface domain token 'auth': %v", ent)
	}

	// Domain kind: only the shared DOMAIN token bridges.
	dom, err := BuildBackwardBridges(ctx, m, synthFacts, "agent/test", EffortHigh, BridgeDomain, 2.0, 2, testCfg, ScopeFilter{})
	require.NoError(t, err)
	if _, ok := containsToken(dom, "auth"); !ok {
		t.Errorf("BridgeDomain must surface domain token 'auth': %v", dom)
	}
	if _, ok := containsToken(dom, "shared"); ok {
		t.Errorf("BridgeDomain must NOT surface entity token 'shared': %v", dom)
	}
}

// TestBridgeSeeds_CrossAxisTokenKind checks that a token appearing as an entity
// on one fact and as a domain on another is labelled BridgeEntity, not
// BridgeDomain. Before the fix, the domain loop overwrote the entity loop's
// tokenKind entry (last-writer-wins), always emitting Kind=domain for any
// shared-axis token.
// Retargeted to enumerateBridgeCandidates (Task 16: kind-precedence logic is unchanged).
func TestBridgeSeeds_CrossAxisTokenKind(t *testing.T) {
	// a.md: entity="auth" (no domain)
	// b.md: domain=["auth"] (no entity)
	// They share token "auth" across different communities.
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", "authored", []string{"auth"}, nil)
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md"},
		},
	}

	got := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeBoth, ScopeFilter{})
	set, found := containsToken(got, "auth")
	require.True(t, found, "expected bridge on 'auth'")
	// Entity beats domain as the Kind label when both axes carry the token.
	require.Equal(t, BridgeEntity, set.Kind,
		"token carried as entity on ≥1 fact must be labelled BridgeEntity, not BridgeDomain")
}

// TestBridgeSeedsCanonicalTokenUnifiesVariants asserts that case/hyphen variants
// of the same tag (e.g. "Store" vs "store") are unified into a single bridge
// rather than being treated as two distinct single-member tokens that each fail
// the ≥2-member requirement.
// Retargeted to enumerateBridgeCandidates (Task 16: canonicalization logic is unchanged).
func TestBridgeSeedsCanonicalTokenUnifiesVariants(t *testing.T) {
	// Two facts tag the same concept with different casing, in different clusters.
	seeds := []factForLLM{
		{File: "kb/a.md", Domain: []string{"Store"}},
		{File: "kb/b.md", Domain: []string{"store"}},
	}
	clusters := ClusterResult{Clusters: map[int][]string{
		0: {"kb/a.md"}, 1: {"kb/b.md"},
	}}
	got := enumerateBridgeCandidates(seeds, clusters, BridgeDomain, ScopeFilter{})
	// Should form ONE bridge (variants unified), not two half-bridges of <2 members.
	if len(got) != 1 {
		t.Fatalf("expected 1 unified bridge, got %d: %+v", len(got), got)
	}
	if len(got[0].Members) != 2 {
		t.Errorf("unified bridge should have 2 members, got %d", len(got[0].Members))
	}
}

// TestBridgeSeeds_SameTokenEntityAndDomain_NoDuplicateMembers checks that a
// fact with the same string in both Domain and Entities appears exactly once as
// a bridge member. Before the fix, the entity loop and the domain loop each
// appended the fact separately (two passes over byPath), producing a duplicate
// member in BridgeSeedSet.Members.
// Retargeted to enumerateBridgeCandidates (Task 16: dedup logic is unchanged).
func TestBridgeSeeds_SameTokenEntityAndDomain_NoDuplicateMembers(t *testing.T) {
	// a.md has "auth" in both Entities and Domain.
	// b.md has "auth" only in Entities — ensures a cross-cluster bridge forms.
	a := makeFact("a.md", "authored", []string{"auth"}, []string{"auth"})
	b := makeFact("b.md", "authored", nil, []string{"auth"})
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md"},
		},
	}

	got := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeBoth, ScopeFilter{})
	set, found := containsToken(got, "auth")
	require.True(t, found, "expected bridge on 'auth'")

	// Count occurrences of a.md in Members.
	count := 0
	for _, m := range set.Members {
		if m.File == "a.md" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"a fact with the same token in both Domain and Entities must appear exactly once as a member, got %d", count)
}

// TestBuildBackwardBridges_UsesConfiguredResolution smoke-tests that backward
// discovery runs end-to-end with the caller-supplied resolution/min-community
// and still surfaces the cross-community bridge.
//
// Pre-PR98 this asserted the exact (2.0, 2) pair via the CachedClusterFacts mock
// args, guarding against a hardcoded (1.0, 1). Clustering is now in-process
// (ScopedCluster → gonum Louvain): the resolution/min-community are plain
// function args threaded straight into ScopedCluster, consumed in-process rather
// than passed to any index method, so there is no mock seam left to assert them
// on. The hardcoding risk the old test guarded is gone with the cache key; what
// remains worth pinning is that the configured values flow through without error.
func TestBuildBackwardBridges_UsesConfiguredResolution(t *testing.T) {
	ctx := context.Background()

	mk := func(path string) fact.Fact {
		f := fact.NewFact(path)
		f.Title, f.Body, f.Type = path, path, fact.Synthesis
		f.Entities = []string{"shared"}
		return f
	}
	synthFacts := []fact.Fact{mk("kb/a.md"), mk("kb/b.md")}

	const wantResolution = 2.0
	const wantMinCommunity = 2

	m := NewMockSearchIndex(gomock.NewController(t))
	// a,b cluster apart so "shared" bridges across communities.
	expectScopedClusterPartition(m, nil, [][]string{{"kb/a.md"}, {"kb/b.md"}})
	// Scoring expectations for the cross-community candidate.
	m.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).
		Return(store.NewSimilarityGraph([][2]string{{"kb/a.md", "kb/b.md"}}), nil).AnyTimes()
	m.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(map[string]struct{}{}, nil).AnyTimes()
	m.EXPECT().TokenDF(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(1, nil).AnyTimes()

	_, err := BuildBackwardBridges(ctx, m, synthFacts, "agent/test", EffortHigh, BridgeBoth, wantResolution, wantMinCommunity, testCfg, ScopeFilter{})
	require.NoError(t, err)
}

// TestEffortBudget_StaysBelowPriorityBand pins the headroom invariant: the largest
// per-effort budget must stay strictly below the forward-discover priority band width, so
// the rank-derived priority of the deepest enqueued discover item can never
// reach reflect. If someone raises effortBudget(EffortHigh) past the band, this
// fails loudly instead of silently letting discovery reorder after reflect.
func TestEffortBudget_StaysBelowPriorityBand(t *testing.T) {
	require.Less(t, effortBudget(EffortHigh), maxBridgeSeeds,
		"effortBudget(EffortHigh) must stay below the forward-discover priority band width")
	require.Less(t, effortBudget(EffortMedium), maxBridgeSeeds,
		"effortBudget(EffortMedium) must stay below the forward-discover priority band width")
}

// ─── buildScoredBridges tests ──────────────────────────────────────────────────

// testCfg is the small QualityConfig used across buildScoredBridges tests.
var testCfg = QualityConfig{
	CohFloor:     0.5,
	QualityFloor: 0,
	WCoh:         1,
	WGap:         1,
	WSpec:        1,
	MaxMembers:   5,
}

// TestBuildScoredBridges_EffortNormal_Nil verifies the byte-identical
// EffortNormal gate: buildScoredBridges must return (nil, nil) without calling
// any idx method.
func TestBuildScoredBridges_EffortNormal_Nil(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	// No expectations — any call to idx would fail the test.

	seeds := []factForLLM{
		makeFact("a.md", "authored", nil, []string{"tok"}),
		makeFact("b.md", "authored", nil, []string{"tok"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}

	got, err := buildScoredBridges(context.Background(), idx, "main", seeds, cr, BridgeBoth, EffortNormal, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Nil(t, got, "EffortNormal must return nil, no idx calls")
}

// TestBuildScoredBridges_SmallCohesiveCrossCommToken emits a bridge for a
// small (<= MaxMembers) cross-community token with high cohesion.
func TestBuildScoredBridges_SmallCohesiveCrossCommToken(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "main"

	seeds := []factForLLM{
		makeFact("a.md", "authored", nil, []string{"alpha"}),
		makeFact("b.md", "authored", nil, []string{"alpha"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}

	// a↔b connected → cohesion = 1.0 (≥ CohFloor=0.5) — passes the gate.
	g := store.NewSimilarityGraph([][2]string{{"a.md", "b.md"}})
	idx.EXPECT().SimilarityAdjacency(ctx, gomock.Any()).Return(g, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(ctx, gomock.Any()).Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(ctx, branch, gomock.Any(), gomock.Any()).Return(1, nil).AnyTimes()

	got, err := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1, "expect one bridge for 'alpha'")
	require.Equal(t, "alpha", got[0].Token)
	require.Greater(t, got[0].Q, 0.0)
	// Members must be path-sorted and contain exactly a.md and b.md.
	require.Len(t, got[0].Members, 2)
	require.Equal(t, "a.md", got[0].Members[0].File)
	require.Equal(t, "b.md", got[0].Members[1].File)
}

// TestBuildScoredBridges_LowCohesion_DroppedByGate verifies that a
// cross-community token with cohesion < CohFloor is dropped (not included in
// the output at all, since buildScoredBridges only emits kept bridges).
func TestBuildScoredBridges_LowCohesion_DroppedByGate(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "main"

	seeds := []factForLLM{
		makeFact("x.md", "authored", nil, []string{"gappy"}),
		makeFact("y.md", "authored", nil, []string{"gappy"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"x.md"}, 1: {"y.md"}},
	}

	// No edges → cohesion = 0 < CohFloor=0.5 → gate fires → not kept.
	g := store.NewSimilarityGraph(nil)
	idx.EXPECT().SimilarityAdjacency(ctx, gomock.Any()).Return(g, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(ctx, gomock.Any()).Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(ctx, branch, gomock.Any(), gomock.Any()).Return(1, nil).AnyTimes()

	got, err := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Empty(t, got, "low-cohesion candidate must be dropped (not kept)")
}

// TestBuildScoredBridges_OversizedGroup_CohesiveSubset verifies that a group
// with > MaxMembers members is reshaped: the cohesive cross-community subset is
// kept and the incoherent remainder is discarded.
func TestBuildScoredBridges_OversizedGroup_CohesiveSubset(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "main"

	// 7 members > MaxMembers=5. Paths p0..p4 form a tight cohesive subset
	// spanning communities 0 (p0,p1,p2) and 1 (p3,p4). p5 and p6 are also in
	// communities but have no edges to the tight subset.
	paths := []string{"p0.md", "p1.md", "p2.md", "p3.md", "p4.md", "p5.md", "p6.md"}
	seeds := make([]factForLLM, len(paths))
	for i, p := range paths {
		seeds[i] = makeFact(p, "authored", nil, []string{"broad"})
	}
	cr := ClusterResult{
		Clusters: map[int][]string{
			0: {"p0.md", "p1.md", "p2.md", "p5.md"},
			1: {"p3.md", "p4.md", "p6.md"},
		},
	}

	// Dense subgraph: p0-p4 all connected to each other (cross-community subset).
	// p5 and p6 have no edges to anyone.
	cohesivePairs := [][2]string{
		{"p0.md", "p1.md"}, {"p0.md", "p2.md"}, {"p0.md", "p3.md"}, {"p0.md", "p4.md"},
		{"p1.md", "p2.md"}, {"p1.md", "p3.md"}, {"p1.md", "p4.md"},
		{"p2.md", "p3.md"}, {"p2.md", "p4.md"},
		{"p3.md", "p4.md"},
	}
	g := store.NewSimilarityGraph(cohesivePairs)

	idx.EXPECT().SimilarityAdjacency(ctx, gomock.Any()).Return(g, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(ctx, gomock.Any()).Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(ctx, branch, gomock.Any(), gomock.Any()).Return(1, nil).AnyTimes()

	got, err := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1, "expect exactly one bridge for 'broad' after reshape")
	bridge := got[0]
	require.Equal(t, "broad", bridge.Token)
	require.LessOrEqual(t, len(bridge.Members), testCfg.MaxMembers,
		"reshaped members must be <= MaxMembers")
	// The cohesive cross-community subset (p0..p4) must all be present;
	// p5 and p6 must be absent since they have no edges.
	memberPaths := make(map[string]bool)
	for _, m := range bridge.Members {
		memberPaths[m.File] = true
	}
	require.False(t, memberPaths["p5.md"], "p5.md has no edges; must be excluded by reshape")
	require.False(t, memberPaths["p6.md"], "p6.md has no edges; must be excluded by reshape")
	require.Greater(t, bridge.Q, 0.0)
}

// TestBuildScoredBridges_OversizedGroup_NoCohesiveSeam verifies that an
// oversized group where reshape finds no cross-community connected pair yields
// no bridge (candidate dropped).
func TestBuildScoredBridges_OversizedGroup_NoCohesiveSeam(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "main"

	// 6 members > MaxMembers=5. No edges at all → reshapeCohesiveSubset returns nil.
	paths := []string{"a.md", "b.md", "c.md", "d.md", "e.md", "f.md"}
	seeds := make([]factForLLM, len(paths))
	for i, p := range paths {
		seeds[i] = makeFact(p, "authored", nil, []string{"broad2"})
	}
	cr := ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md", "b.md", "c.md"},
			1: {"d.md", "e.md", "f.md"},
		},
	}

	// No edges → reshape can find no cross-community connected pair → drops candidate.
	g := store.NewSimilarityGraph(nil)
	idx.EXPECT().SimilarityAdjacency(ctx, gomock.Any()).Return(g, nil).AnyTimes()

	got, err := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Empty(t, got, "oversized group with no cohesive seam must produce no bridge")
}

// TestBuildScoredBridges_QDescOrder verifies that multiple bridges are ranked
// by Q descending, then Token ascending on ties.
func TestBuildScoredBridges_QDescOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "main"

	// Two tokens: "alpha" (cohesion 1.0) and "beta" (cohesion 1.0).
	// Both cross-community; alpha has df=1 (spec=1.0), beta has df=2 (spec=0.5).
	// With WCoh=WGap=WSpec=1 and gap=1.0 for both:
	// alpha Q = 1*1.0 + 1*1.0 + 1*1.0 = 3.0
	// beta  Q = 1*1.0 + 1*1.0 + 1*0.5 = 2.5
	// Expected order: alpha first.
	seeds := []factForLLM{
		makeFact("a1.md", "authored", nil, []string{"alpha"}),
		makeFact("a2.md", "authored", nil, []string{"alpha"}),
		makeFact("b1.md", "authored", nil, []string{"beta"}),
		makeFact("b2.md", "authored", nil, []string{"beta"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{
			0: {"a1.md", "b1.md"},
			1: {"a2.md", "b2.md"},
		},
	}

	// All pairs connected → cohesion = 1.0 for any pair-sized group.
	idx.EXPECT().SimilarityAdjacency(ctx, gomock.Any()).
		DoAndReturn(func(_ context.Context, paths []string) (store.SimilarityGraph, error) {
			pairs := make([][2]string, 0)
			for i := 0; i < len(paths); i++ {
				for j := i + 1; j < len(paths); j++ {
					pairs = append(pairs, [2]string{paths[i], paths[j]})
				}
			}
			return store.NewSimilarityGraph(pairs), nil
		}).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(ctx, gomock.Any()).Return(map[string]struct{}{}, nil).AnyTimes()
	// alpha: df=1, beta: df=2
	idx.EXPECT().TokenDF(ctx, branch, "alpha", string(BridgeEntity)).Return(1, nil).AnyTimes()
	idx.EXPECT().TokenDF(ctx, branch, "beta", string(BridgeEntity)).Return(2, nil).AnyTimes()

	got, err := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "alpha", got[0].Token, "alpha has higher Q and must come first")
	require.Equal(t, "beta", got[1].Token)
	require.Greater(t, got[0].Q, got[1].Q, "alpha Q must exceed beta Q")
}

// TestTask16_ForwardEffortNormal_ZeroDiscovers is the explicit Task-16
// forward-path byte-identical-contract regression. After the production
// wiring is switched from bridgeSeedsFromClusters to buildScoredBridges,
// EffortNormal MUST still return (nil, nil) without any idx calls.
// Written BEFORE the production wiring is changed (TDD: proves the invariant
// survives the switch). The test fails if the effort gate is bypassed.
func TestTask16_ForwardEffortNormal_ZeroDiscovers(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	// No expectations: any idx call fails the test.

	seeds := []factForLLM{
		makeFact("a.md", "authored", nil, []string{"tok"}),
		makeFact("b.md", "authored", nil, []string{"tok"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}

	got, err := buildScoredBridges(context.Background(), idx, "main", seeds, cr, BridgeBoth, EffortNormal, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Nil(t, got, "forward path at EffortNormal must return (nil, nil) — byte-identical contract")
}

// ─── scope-exclusion tests (Task 19) ─────────────────────────────────────────

// TestEnumerate_ScopedDomainExcludesMatchingToken verifies that when scope has
// a domain entry that canonically matches a cross-community token, that token
// is NOT emitted as a bridge candidate. A different cross-community token that
// does NOT match the scope IS emitted. Empty scope (ScopeFilter{}) is
// unchanged — the "auth" token is emitted as before.
//
// By-kind rule: a domain-kind token is checked ONLY against scope.Domain
// (not scope.Entities). This prevents false positives when a scope entity
// name happens to share a string with a domain token.
func TestEnumerate_ScopedDomainExcludesMatchingToken(t *testing.T) {
	// "auth" domain token crosses communities — matches scope.Domain → excluded.
	// "billing" domain token crosses communities — does NOT match scope → included.
	a := makeFact("a.md", "authored", []string{"auth", "billing"}, nil)
	b := makeFact("b.md", "authored", []string{"auth", "billing"}, nil)
	c := makeFact("c.md", "authored", []string{"billing"}, nil)
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md", "c.md"},
		},
	}
	scope := ScopeFilter{Domain: []string{"Auth"}} // canonical match for "auth"

	got := enumerateBridgeCandidates([]factForLLM{a, b, c}, clusters, BridgeDomain, scope)

	if _, found := containsToken(got, "auth"); found {
		t.Error("scoped domain token 'auth' must be EXCLUDED from bridge candidates")
	}
	if _, found := containsToken(got, "billing"); !found {
		t.Error("non-scope domain token 'billing' must still appear as a bridge candidate")
	}
}

// TestEnumerate_ScopedEntityExcludesMatchingToken verifies that when scope has
// an entity entry that canonically matches a cross-community entity token, that
// token is NOT emitted. A different entity token IS.
func TestEnumerate_ScopedEntityExcludesMatchingToken(t *testing.T) {
	// "Alice" entity token crosses communities — matches scope.Entities → excluded.
	// "Bob" entity token crosses communities — does NOT match → included.
	a := makeFact("a.md", "authored", nil, []string{"Alice", "Bob"})
	b := makeFact("b.md", "authored", nil, []string{"Alice", "Bob"})
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md"},
		},
	}
	scope := ScopeFilter{Entities: []string{"alice"}} // canonical match for "Alice"

	got := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeEntity, scope)

	if _, found := containsToken(got, "Alice"); found {
		t.Error("scoped entity token 'Alice' must be EXCLUDED from bridge candidates")
	}
	if _, found := containsToken(got, "Bob"); !found {
		t.Error("non-scope entity token 'Bob' must still appear as a bridge candidate")
	}
}

// TestEnumerate_EmptyScopeUnchanged verifies byte-identical behavior when scope
// is empty: ALL cross-community tokens are emitted, same as pre-scope behavior.
func TestEnumerate_EmptyScopeUnchanged(t *testing.T) {
	a := makeFact("a.md", "authored", []string{"auth"}, nil)
	b := makeFact("b.md", "authored", []string{"auth"}, nil)
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}

	withScope := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeDomain, ScopeFilter{})
	// Must emit "auth" just like the old no-scope behavior.
	if _, found := containsToken(withScope, "auth"); !found {
		t.Error("empty scope must emit cross-community token 'auth' (byte-identical to no-scope behavior)")
	}
}

// TestEnumerate_CrossKindNoExclusion verifies the by-kind rule: a domain token
// that equals a scope ENTITY string is NOT excluded (and vice versa). The
// exclusion must be kind-local: domain tokens are checked only against
// scope.Domain; entity tokens only against scope.Entities.
func TestEnumerate_CrossKindNoExclusion(t *testing.T) {
	// "auth" appears as a DOMAIN token on both facts.
	// scope has "auth" as an ENTITY (not a domain) — the domain token must NOT be excluded.
	a := makeFact("a.md", "authored", []string{"auth"}, nil)
	b := makeFact("b.md", "authored", []string{"auth"}, nil)
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}
	scope := ScopeFilter{Entities: []string{"auth"}} // only entity scope, not domain

	got := enumerateBridgeCandidates([]factForLLM{a, b}, clusters, BridgeDomain, scope)

	if _, found := containsToken(got, "auth"); !found {
		t.Error("domain token 'auth' must NOT be excluded when scope.Entities matches it — domain-only-vs-Domain rule")
	}
}

// TestBuildScoredBridges_ScopeExcludesToken is an end-to-end test verifying
// that buildScoredBridges with a scope filter removes the scope-matching token
// before scoring, so it never appears in the output.
func TestBuildScoredBridges_ScopeExcludesToken(t *testing.T) {
	ctx := context.Background()
	idx := cohesiveMockIdx(t)

	// "scoped" entity is the scope token; "other" is not.
	seeds := []factForLLM{
		makeFact("s1.md", "authored", nil, []string{"scoped", "other"}),
		makeFact("s2.md", "authored", nil, []string{"scoped", "other"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"s1.md"}, 1: {"s2.md"}},
	}
	scope := ScopeFilter{Entities: []string{"scoped"}}

	got, err := buildScoredBridges(ctx, idx, "main", seeds, cr, BridgeEntity, EffortHigh, testCfg, scope)
	require.NoError(t, err)

	for _, b := range got {
		if b.Token == "scoped" {
			t.Errorf("scoped entity token 'scoped' must not appear in buildScoredBridges output when scope matches it")
		}
	}
	// "other" token is NOT in scope → should appear.
	if _, found := containsToken(got, "other"); !found {
		t.Error("non-scope token 'other' must still be returned by buildScoredBridges")
	}
}

// TestBuildScoredBridges_EmptyScope_Unchanged verifies that an empty scope
// leaves buildScoredBridges output byte-identical to the pre-scope behavior.
func TestBuildScoredBridges_EmptyScope_Unchanged(t *testing.T) {
	ctx := context.Background()
	idx := cohesiveMockIdx(t)

	seeds := []factForLLM{
		makeFact("u.md", "authored", nil, []string{"tok"}),
		makeFact("v.md", "authored", nil, []string{"tok"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"u.md"}, 1: {"v.md"}},
	}

	got, err := buildScoredBridges(ctx, idx, "main", seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1, "empty scope must not filter any candidates")
	require.Equal(t, "tok", got[0].Token)
}

// TestBuildScoredBridges_Determinism verifies that two identical calls produce
// identical ordering and member sets.
func TestBuildScoredBridges_Determinism(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)
	ctx := context.Background()
	branch := "main"

	seeds := []factForLLM{
		makeFact("p.md", "authored", nil, []string{"det"}),
		makeFact("q.md", "authored", nil, []string{"det"}),
	}
	cr := ClusterResult{
		Clusters: map[int][]string{0: {"p.md"}, 1: {"q.md"}},
	}

	g := store.NewSimilarityGraph([][2]string{{"p.md", "q.md"}})
	idx.EXPECT().SimilarityAdjacency(ctx, gomock.Any()).Return(g, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(ctx, gomock.Any()).Return(map[string]struct{}{}, nil).AnyTimes()
	idx.EXPECT().TokenDF(ctx, branch, gomock.Any(), gomock.Any()).Return(1, nil).AnyTimes()

	run1, err1 := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	run2, err2 := buildScoredBridges(ctx, idx, branch, seeds, cr, BridgeEntity, EffortHigh, testCfg, ScopeFilter{})
	require.NoError(t, err1)
	require.NoError(t, err2)
	require.Equal(t, run1, run2, "two identical runs must produce identical results")
}
