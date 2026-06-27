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
func TestBridgeSeeds_CrossClusterEntity(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", "authored", nil, []string{"auth"})
	c := makeFact("c.md", "authored", nil, []string{"auth"})
	d := makeFact("d.md", "authored", nil, []string{"misc"})
	seeds := []factForLLM{a, b, c, d}

	// a in community 0, b in community 1, c in community 0 (same as a).
	// d in community 1 (same as b) but carries a different entity.
	clusters := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md", "c.md"},
			1: {"b.md", "d.md"},
		},
	}

	got := bridgeSeeds(seeds, clusters, BridgeEntity, EffortHigh, true)

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

// TestBridgeSeeds_NormalEffortEmpty asserts that EffortNormal returns nil —
// the discovery engine never engages.
func TestBridgeSeeds_NormalEffortEmpty(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", "authored", nil, []string{"auth"})
	clusters := store.ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}
	got := bridgeSeeds([]factForLLM{a, b}, clusters, BridgeBoth, EffortNormal, true)
	if got != nil {
		t.Errorf("EffortNormal must return nil, got %v", got)
	}
}

// TestBridgeSeeds_ExcludesDiscoveredOrigin enforces Plan 03 §7 idempotency:
// a fact whose origin is 'discovered' is NEVER selected as a seed, even if
// it would otherwise complete a bridge.
func TestBridgeSeeds_ExcludesDiscoveredOrigin(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", string(fact.Discovered), nil, []string{"auth"})
	clusters := store.ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}
	got := bridgeSeeds([]factForLLM{a, b}, clusters, BridgeBoth, EffortHigh, true)
	if len(got) != 0 {
		t.Errorf("discovered seed must be excluded; b alone leaves a w/o partner: %+v", got)
	}
}

// TestBridgeSeeds_EffortBudget asserts the unscoped pool is truncated to
// the effort budget while a scoped pool is not.
func TestBridgeSeeds_EffortBudget(t *testing.T) {
	// Build 60 distinct cross-cluster bridges by creating 60 unique tokens,
	// each appearing on a fact in community 0 and a fact in community 1.
	var seeds []factForLLM
	cluster0 := []string{}
	cluster1 := []string{}
	for i := 0; i < 60; i++ {
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
	clusters := store.ClusterResult{
		Clusters: map[int][]string{0: cluster0, 1: cluster1},
	}

	med := bridgeSeeds(seeds, clusters, BridgeEntity, EffortMedium, false)
	hi := bridgeSeeds(seeds, clusters, BridgeEntity, EffortHigh, false)
	scoped := bridgeSeeds(seeds, clusters, BridgeEntity, EffortHigh, true)

	if len(med) != effortBudget(EffortMedium) {
		t.Errorf("medium budget: got %d, want %d", len(med), effortBudget(EffortMedium))
	}
	if len(hi) != effortBudget(EffortHigh) {
		t.Errorf("high budget: got %d, want %d", len(hi), effortBudget(EffortHigh))
	}
	if len(scoped) != 60 {
		t.Errorf("scoped (filtered) pool must skip budget truncation: got %d, want 60", len(scoped))
	}
}

// TestBridgeSeeds_AbsoluteCapEvenWhenScoped is the regression guard for the
// priority-band overflow: a scoped pool skips the per-effort budget, so before
// the cap it could surface arbitrarily many bridges. Each forward bridge gets
// priority forwardDiscoverPriorityBase-rank; past maxBridgeSeeds the rank-
// derived priority reaches reflect's -100 and discovery reorders after reflect.
// The absolute maxBridgeSeeds backstop must apply even when scoped=true.
func TestBridgeSeeds_AbsoluteCapEvenWhenScoped(t *testing.T) {
	n := maxBridgeSeeds + 25 // comfortably over the cap
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
	clusters := store.ClusterResult{Clusters: map[int][]string{0: cluster0, 1: cluster1}}

	scoped := bridgeSeeds(seeds, clusters, BridgeEntity, EffortHigh, true)
	if len(scoped) != maxBridgeSeeds {
		t.Fatalf("scoped pool must still be capped at maxBridgeSeeds: got %d, want %d", len(scoped), maxBridgeSeeds)
	}
	// The lowest-priority surviving item must stay strictly above reflect.
	lowest := forwardDiscoverPriority(len(scoped) - 1)
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
func TestBridgeSeeds_DomainOnly_NotEntity(t *testing.T) {
	a := makeFact("a.md", "authored", []string{"auth"}, []string{"X"})
	b := makeFact("b.md", "authored", []string{"auth"}, []string{"Y"})
	clusters := store.ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}, 1: {"b.md"}},
	}

	dom := bridgeSeeds([]factForLLM{a, b}, clusters, BridgeDomain, EffortHigh, true)
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
func TestBridgeSeeds_OrphanSeedNotCommunityZero(t *testing.T) {
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	orphan := makeFact("orphan.md", "authored", nil, []string{"auth"})
	// a.md is in real community 0; orphan.md is in no cluster and no noise list.
	clusters := store.ClusterResult{
		Clusters: map[int][]string{0: {"a.md"}},
	}

	got := bridgeSeeds([]factForLLM{a, orphan}, clusters, BridgeEntity, EffortHigh, true)
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
func TestBuildBackwardBridges_HonorsBridgeKind(t *testing.T) {
	ctx := context.Background()

	mk := func(path string) fact.Fact {
		f := fact.NewFact(path)
		f.Title, f.Body, f.Type = path, path, fact.Synthesis
		f.Domain, f.Entities = []string{"auth"}, []string{"shared"}
		return f
	}
	synthFacts := []fact.Fact{mk("kb/a.md"), mk("kb/b.md")}

	cr := store.ClusterResult{Clusters: map[int][]string{0: {"kb/a.md"}, 1: {"kb/b.md"}}}
	m := NewMockSearchIndex(gomock.NewController(t))
	m.EXPECT().CachedClusterFacts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(cr, nil).AnyTimes()

	// Entity kind: only the shared ENTITY token bridges.
	ent, err := BuildBackwardBridges(ctx, m, synthFacts, "agent/test", EffortHigh, ScopeFilter{}, BridgeEntity, 2.0, 2)
	require.NoError(t, err)
	if _, ok := containsToken(ent, "shared"); !ok {
		t.Errorf("BridgeEntity must surface entity token 'shared': %v", ent)
	}
	if _, ok := containsToken(ent, "auth"); ok {
		t.Errorf("BridgeEntity must NOT surface domain token 'auth': %v", ent)
	}

	// Domain kind: only the shared DOMAIN token bridges.
	dom, err := BuildBackwardBridges(ctx, m, synthFacts, "agent/test", EffortHigh, ScopeFilter{}, BridgeDomain, 2.0, 2)
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
func TestBridgeSeeds_CrossAxisTokenKind(t *testing.T) {
	// a.md: entity="auth" (no domain)
	// b.md: domain=["auth"] (no entity)
	// They share token "auth" across different communities.
	a := makeFact("a.md", "authored", nil, []string{"auth"})
	b := makeFact("b.md", "authored", []string{"auth"}, nil)
	clusters := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md"},
		},
	}

	got := bridgeSeeds([]factForLLM{a, b}, clusters, BridgeBoth, EffortHigh, true)
	set, found := containsToken(got, "auth")
	require.True(t, found, "expected bridge on 'auth'")
	// Entity beats domain as the Kind label when both axes carry the token.
	require.Equal(t, BridgeEntity, set.Kind,
		"token carried as entity on ≥1 fact must be labelled BridgeEntity, not BridgeDomain")
}

// TestBridgeSeeds_SameTokenEntityAndDomain_NoDuplicateMembers checks that a
// fact with the same string in both Domain and Entities appears exactly once as
// a bridge member. Before the fix, the entity loop and the domain loop each
// appended the fact separately (two passes over byPath), producing a duplicate
// member in BridgeSeedSet.Members.
func TestBridgeSeeds_SameTokenEntityAndDomain_NoDuplicateMembers(t *testing.T) {
	// a.md has "auth" in both Entities and Domain.
	// b.md has "auth" only in Entities — ensures a cross-cluster bridge forms.
	a := makeFact("a.md", "authored", []string{"auth"}, []string{"auth"})
	b := makeFact("b.md", "authored", nil, []string{"auth"})
	clusters := store.ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md"},
		},
	}

	got := bridgeSeeds([]factForLLM{a, b}, clusters, BridgeBoth, EffortHigh, true)
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

// TestBuildBackwardBridges_UsesConfiguredResolution guards that backward
// discovery clusters with the resolution/min-community it is GIVEN (the shared
// cluster config knob), not a hardcoded value. The mock expects exactly the
// configured (2.0, 2) pair — if the old hardcoded (1.0, 1) ever returns, the
// call arrives with unexpected arguments and the test fails.
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

	cr := store.ClusterResult{Clusters: map[int][]string{0: {"kb/a.md"}, 1: {"kb/b.md"}}}
	m := NewMockSearchIndex(gomock.NewController(t))
	// Exact-arg expectation: clustering MUST use the passed-in config values.
	m.EXPECT().
		CachedClusterFacts(gomock.Any(), "agent/test", wantResolution, wantMinCommunity).
		Return(cr, nil).
		Times(1)

	_, err := BuildBackwardBridges(ctx, m, synthFacts, "agent/test", EffortHigh, ScopeFilter{}, BridgeBoth, wantResolution, wantMinCommunity)
	require.NoError(t, err)
}
