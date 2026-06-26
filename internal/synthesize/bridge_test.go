package synthesize

import (
	"testing"

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

func tokenName(i int) string {
	// 60 tokens — enough variety, deterministic ordering.
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
