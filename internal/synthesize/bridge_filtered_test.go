package synthesize

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// --- specificityWithinScope tests ---

// TestSpecificityWithinScope_TokenOnOneFact verifies that a token carried by
// exactly 1 fact out of N yields spec = 1.0 (df=1, 1/1 = 1.0).
func TestSpecificityWithinScope_TokenOnOneFact(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"store"}, Entities: []string{}},
		{File: "b.md", Domain: []string{"query"}, Entities: []string{}},
		{File: "c.md", Domain: []string{"index"}, Entities: []string{}},
	}
	got := specificityWithinScope("store", BridgeDomain, pool)
	if got != 1.0 {
		t.Fatalf("want 1.0, got %f", got)
	}
}

// TestSpecificityWithinScope_TokenOnFourFacts verifies that a token carried by
// 4 pool facts yields spec = 0.25 (df=4, 1/4 = 0.25).
func TestSpecificityWithinScope_TokenOnFourFacts(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Entities: []string{"Go"}},
		{File: "b.md", Entities: []string{"Go"}},
		{File: "c.md", Entities: []string{"Go"}},
		{File: "d.md", Entities: []string{"Go"}},
		{File: "e.md", Entities: []string{"Rust"}},
	}
	got := specificityWithinScope("Go", BridgeEntity, pool)
	want := 0.25
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// TestSpecificityWithinScope_TokenAbsent verifies that a token absent from
// the pool yields 1.0 (max(df,1) = 1, 1/1 = 1.0).
func TestSpecificityWithinScope_TokenAbsent(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"store"}, Entities: []string{}},
	}
	got := specificityWithinScope("nosuchtoken", BridgeDomain, pool)
	if got != 1.0 {
		t.Fatalf("want 1.0, got %f", got)
	}
}

// TestSpecificityWithinScope_DomainHierarchyMatch verifies that a query token
// "store" matches a fact domain "store/sqlite" via DomainTagMatches hierarchy.
func TestSpecificityWithinScope_DomainHierarchyMatch(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"store/sqlite"}},
		{File: "b.md", Domain: []string{"store/postgres"}},
		{File: "c.md", Domain: []string{"query"}},
	}
	// "store" is parent of "store/sqlite" and "store/postgres" — df should be 2.
	got := specificityWithinScope("store", BridgeDomain, pool)
	want := 0.5 // 1/2
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// TestSpecificityWithinScope_EntityCaseInsensitive verifies that entity
// matching is case-insensitive (EntityTagMatches uses case-fold equality).
func TestSpecificityWithinScope_EntityCaseInsensitive(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Entities: []string{"Anthropic"}},
		{File: "b.md", Entities: []string{"anthropic"}},
		{File: "c.md", Entities: []string{"OpenAI"}},
	}
	// "Anthropic" matches both "Anthropic" and "anthropic" via case-fold — df=2.
	got := specificityWithinScope("Anthropic", BridgeEntity, pool)
	want := 0.5
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// TestSpecificityWithinScope_BridgeBothMatchesBothAxes verifies that
// BridgeBoth (or "") matches on either domain or entity axis.
func TestSpecificityWithinScope_BridgeBothMatchesBothAxes(t *testing.T) {
	pool := []factForLLM{
		{File: "a.md", Domain: []string{"llm"}, Entities: []string{}},
		{File: "b.md", Domain: []string{}, Entities: []string{"llm"}},
		{File: "c.md", Domain: []string{"store"}, Entities: []string{}},
	}
	// "llm" appears once in domain (a.md) and once in entity (b.md) — df=2 for BridgeBoth.
	got := specificityWithinScope("llm", BridgeBoth, pool)
	want := 0.5
	if got != want {
		t.Fatalf("want %f, got %f", want, got)
	}
}

// --- sharedSubToken tests ---

// TestSharedSubToken_RarestSharedWins verifies that when members all share
// both "x" (df=2 in pool, rare) and "y" (df=10, common), the returned token
// is "x" with spec = 1/2 = 0.5.
func TestSharedSubToken_RarestSharedWins(t *testing.T) {
	// Build a pool where "x" appears in 2 facts and "y" in 10.
	pool := make([]factForLLM, 0, 12)
	pool = append(pool, factForLLM{File: "x1.md", Domain: []string{"x"}})
	pool = append(pool, factForLLM{File: "x2.md", Domain: []string{"x"}})
	for i := 0; i < 10; i++ {
		pool = append(pool, factForLLM{File: "y" + string(rune('0'+i)) + ".md", Domain: []string{"y"}})
	}

	// Members all carry both "x" and "y".
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"x", "y"}},
		{File: "m2.md", Domain: []string{"x", "y"}},
		{File: "m3.md", Domain: []string{"x", "y"}},
	}

	tok, kind, spec := sharedSubToken(members, BridgeDomain, pool)
	if tok != "x" {
		t.Fatalf("want token \"x\", got %q", tok)
	}
	if kind != BridgeDomain {
		t.Fatalf("want kind BridgeDomain, got %q", kind)
	}
	want := 0.5 // 1/2
	if spec != want {
		t.Fatalf("want spec %f, got %f", want, spec)
	}
}

// TestSharedSubToken_NoSharedToken verifies that when members share no token,
// the function returns ("", BridgeBoth, neutralSpec).
func TestSharedSubToken_NoSharedToken(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"alpha"}},
		{File: "p2.md", Domain: []string{"beta"}},
	}
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"alpha"}},
		{File: "m2.md", Domain: []string{"beta"}},
	}

	tok, _, spec := sharedSubToken(members, BridgeDomain, pool)
	if tok != "" {
		t.Fatalf("want empty token, got %q", tok)
	}
	if spec != neutralSpec {
		t.Fatalf("want neutralSpec %f, got %f", neutralSpec, spec)
	}
}

// TestSharedSubToken_CanonicalUnification verifies that "Store" and "store"
// are treated as the same token (canonical unification via CanonicalizeTag).
func TestSharedSubToken_CanonicalUnification(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"store"}},
	}
	// m1 carries "Store", m2 carries "store" — canonical form is the same.
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"Store"}},
		{File: "m2.md", Domain: []string{"store"}},
	}

	tok, kind, spec := sharedSubToken(members, BridgeDomain, pool)
	if tok == "" {
		t.Fatalf("want a shared token, got empty (canonical unification failed)")
	}
	if kind != BridgeDomain {
		t.Fatalf("want BridgeDomain, got %q", kind)
	}
	// df=1 (only p1.md carries "store" in pool, members are NOT in pool here)
	if spec != 1.0 {
		t.Fatalf("want spec 1.0 (df=1), got %f", spec)
	}
}

// TestSharedSubToken_DeterministicTieBreak verifies that when two shared tokens
// have equal df in pool, the one with the smallest canonical string is returned.
func TestSharedSubToken_DeterministicTieBreak(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"aaa", "zzz"}},
	}
	// Both "aaa" and "zzz" appear in 1 fact each — equal df.
	// Members carry both. Tie-break: smallest canonical string = "aaa".
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"aaa", "zzz"}},
		{File: "m2.md", Domain: []string{"aaa", "zzz"}},
	}

	tok, _, _ := sharedSubToken(members, BridgeDomain, pool)
	if tok != "aaa" {
		t.Fatalf("want \"aaa\" (tie-break: smallest canonical), got %q", tok)
	}
}

// TestSharedSubToken_EntityBeatesDomainOnBridgeBoth verifies that when a
// token appears on both entity and domain axes under BridgeBoth, the returned
// kind is BridgeEntity (entity beats domain, mirroring enumerateBridgeCandidates).
func TestSharedSubToken_EntityBeatesDomainOnBridgeBoth(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Entities: []string{"llm"}, Domain: []string{"llm"}},
	}
	members := []factForLLM{
		{File: "m1.md", Entities: []string{"llm"}, Domain: []string{"llm"}},
		{File: "m2.md", Entities: []string{"llm"}, Domain: []string{"llm"}},
	}

	_, kind, _ := sharedSubToken(members, BridgeBoth, pool)
	if kind != BridgeEntity {
		t.Fatalf("want BridgeEntity (entity beats domain under BridgeBoth), got %q", kind)
	}
}

// TestSharedSubToken_SingleMember verifies the len<2 edge: a single member
// yields neutral (no meaningful "shared" set for size-1 slices).
func TestSharedSubToken_SingleMember(t *testing.T) {
	pool := []factForLLM{
		{File: "p1.md", Domain: []string{"store"}},
	}
	members := []factForLLM{
		{File: "m1.md", Domain: []string{"store"}},
	}

	tok, _, spec := sharedSubToken(members, BridgeDomain, pool)
	// Single member: documented contract is the neutral result — empty token
	// and neutralSpec — because callers key on tok == "" for the neutral case.
	// The filtered generator only calls this for subsets of size >= 2 in practice.
	if tok != "" {
		t.Fatalf("want empty token for single member, got %q", tok)
	}
	if spec != neutralSpec {
		t.Fatalf("want neutralSpec for single member, got %f", spec)
	}
}

// TestSharedSubToken_EmptyMembers verifies that empty members returns neutral.
func TestSharedSubToken_EmptyMembers(t *testing.T) {
	pool := []factForLLM{{File: "p1.md", Domain: []string{"store"}}}
	tok, _, spec := sharedSubToken(nil, BridgeDomain, pool)
	if tok != "" {
		t.Fatalf("want empty token for empty members, got %q", tok)
	}
	if spec != neutralSpec {
		t.Fatalf("want neutralSpec for empty members, got %f", spec)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// buildFilteredBridges tests
// ─────────────────────────────────────────────────────────────────────────────

// filteredCfg is a permissive QualityConfig for the filtered-bridge tests:
// CohFloor=0 so any cross-community pair is cohesive enough, QualityFloor=0
// so anything that passes the gate is kept, and MaxMembers=10 so we never
// hit the size gate in these small tests.
var filteredCfg = QualityConfig{
	CohFloor:     0.0,
	QualityFloor: 0.0,
	WCoh:         1.0,
	WGap:         1.0,
	WSpec:        1.0,
	MaxMembers:   10,
}

// buildTwoClusterSeeds returns four seeds in two disjoint communities.
// {a,b} are in community 0 and 1 respectively; {c,d} in communities 2 and 3.
// No edges connect the {a,b} group to the {c,d} group — they are disjoint.
func buildTwoClusterSeeds() (seeds []factForLLM, clusters ClusterResult, g store.SimilarityGraph) {
	seeds = []factForLLM{
		{File: "a.md", Domain: []string{"auth"}, Entities: []string{"UserService"}},
		{File: "b.md", Domain: []string{"auth"}, Entities: []string{"TokenStore"}},
		{File: "c.md", Domain: []string{"billing"}, Entities: []string{"Invoice"}},
		{File: "d.md", Domain: []string{"billing"}, Entities: []string{"Payment"}},
	}
	// a↔b connected; c↔d connected; no cross-group edges.
	g = store.NewSimilarityGraph([][2]string{
		{"a.md", "b.md"},
		{"c.md", "d.md"},
	})
	clusters = ClusterResult{
		Clusters: map[int][]string{
			0: {"a.md"},
			1: {"b.md"},
			2: {"c.md"},
			3: {"d.md"},
		},
	}
	return
}

// TestBuildFilteredBridges_EffortNormal_NilNoIdx verifies the §1 gate:
// EffortNormal returns (nil, nil) immediately without calling idx at all.
func TestBuildFilteredBridges_EffortNormal_NilNoIdx(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	// NO expectations set — any idx call would cause the mock to fail.

	seeds, clusters, _ := buildTwoClusterSeeds()
	scope := ScopeFilter{Domain: []string{"auth"}}

	got, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, scope, EffortNormal, filteredCfg,
	)
	if err != nil {
		t.Fatalf("want nil err, got %v", err)
	}
	if got != nil {
		t.Fatalf("want nil result for EffortNormal, got %v", got)
	}
}

// TestBuildFilteredBridges_TwoDisjointClusters_TwoBridges verifies the core
// iterative-extraction pipeline: a pool with two disjoint cross-community
// cohesive pairs yields two BridgeSeedSets — one per pair — with the correct
// Members.
func TestBuildFilteredBridges_TwoDisjointClusters_TwoBridges(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	seeds, clusters, g := buildTwoClusterSeeds()

	idx := NewMockSearchIndex(ctrl)
	// SimilarityAdjacency is called once with all 4 paths (sorted).
	// ReverseDependentPaths is called for each unique path (derivationGap).
	allPaths := []string{"a.md", "b.md", "c.md", "d.md"}
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.InAnyOrder(allPaths)).Return(g, nil)
	emptyRevdeps := map[string]struct{}{}
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "a.md").Return(emptyRevdeps, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "b.md").Return(emptyRevdeps, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "c.md").Return(emptyRevdeps, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "d.md").Return(emptyRevdeps, nil).AnyTimes()

	scope := ScopeFilter{Domain: []string{"auth"}} // scope is "auth"

	got, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, scope, EffortHigh, filteredCfg,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 bridges, got %d: %+v", len(got), got)
	}

	// Collect member-path sets across the two bridges.
	memberPaths := func(b BridgeSeedSet) []string {
		paths := make([]string, len(b.Members))
		for i, m := range b.Members {
			paths[i] = m.File
		}
		return paths
	}
	set0 := memberPaths(got[0])
	set1 := memberPaths(got[1])

	// The two bridges must together cover {a,b} and {c,d} (order may vary by Q).
	wantSets := [][]string{{"a.md", "b.md"}, {"c.md", "d.md"}}
	matched := 0
	for _, want := range wantSets {
		for _, got := range [][]string{set0, set1} {
			if pathSetsEqual(want, got) {
				matched++
				break
			}
		}
	}
	if matched != 2 {
		t.Fatalf("want bridges {a,b} and {c,d}, got %v and %v", set0, set1)
	}
}

// TestBuildFilteredBridges_RareSharedToken_TokenSet verifies that when a
// subset's members all share a rare sub-token (not the scope token), the
// returned BridgeSeedSet has that token set with within-scope specificity.
//
// Setup: pool has 4 seeds total. e.md and f.md both carry entity "Stripe" and
// domain "billing". Two extra seeds (g2.md, h2.md) also carry "billing" but NOT
// "Stripe". This makes billing df=4 and Stripe df=2, so "Stripe" is rarer and
// wins the sharedSubToken selection over "billing" (the scope domain token).
// "billing" would have been scope-excluded anyway, but "Stripe" wins on rarity
// first, so the bridge emits with Token="Stripe".
func TestBuildFilteredBridges_RareSharedToken_TokenSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// e.md and f.md: carry "Stripe" (entity) + "billing" (domain, scope token).
	// g2.md and h2.md: carry ONLY "billing" — inflate billing df to 4.
	// g2 and h2 are in communities 2 and 3 but not connected, so reshape finds
	// {e,f} as the first (and only) cross-community cohesive seam.
	seeds := []factForLLM{
		{File: "e.md", Domain: []string{"billing"}, Entities: []string{"Stripe"}},
		{File: "f.md", Domain: []string{"billing"}, Entities: []string{"Stripe"}},
		{File: "g2.md", Domain: []string{"billing"}, Entities: []string{}},
		{File: "h2.md", Domain: []string{"billing"}, Entities: []string{}},
	}
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"e.md"},
			1: {"f.md"},
			2: {"g2.md"},
			3: {"h2.md"},
		},
	}
	// Only e↔f are connected; g2, h2 have no edges → reshape finds {e,f} first.
	g := store.NewSimilarityGraph([][2]string{{"e.md", "f.md"}})

	idx := NewMockSearchIndex(ctrl)
	allPaths := []string{"e.md", "f.md", "g2.md", "h2.md"}
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.InAnyOrder(allPaths)).Return(g, nil)
	emptyRevdeps := map[string]struct{}{}
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).Return(emptyRevdeps, nil).AnyTimes()

	// Scope is "billing" (domain) — so "billing" shared token is scope-excluded.
	// "Stripe" is entity (df=2 in pool of 4, rarer than billing df=4) and not in
	// scope → should become the bridge token.
	scope := ScopeFilter{Domain: []string{"billing"}}

	got, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, scope, EffortHigh, filteredCfg,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) < 1 {
		t.Fatalf("want at least 1 bridge, got %d: %+v", len(got), got)
	}
	// First bridge should be {e,f} with Token="Stripe".
	var stripeBridge *BridgeSeedSet
	for i := range got {
		if pathSetsEqual([]string{"e.md", "f.md"}, func() []string {
			ps := make([]string, len(got[i].Members))
			for j, m := range got[i].Members {
				ps[j] = m.File
			}
			return ps
		}()) {
			stripeBridge = &got[i]
			break
		}
	}
	if stripeBridge == nil {
		t.Fatalf("bridge {e.md, f.md} not found in results: %+v", got)
	}
	if stripeBridge.Token != "Stripe" {
		t.Fatalf("want token \"Stripe\", got %q (scope-token exclusion may have wrongly cleared it, or billing won tie-break)", stripeBridge.Token)
	}
}

// TestBuildFilteredBridges_ScopeTokenExcluded_EmptyToken verifies that when
// the only shared token among subset members IS the scope token, Token is set
// to "" (excluded as label) but the bridge is still emitted (cohesive
// cross-community pair passes the gate).
func TestBuildFilteredBridges_ScopeTokenExcluded_EmptyToken(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Two facts; both carry ONLY "auth" as domain (= the scope token). No entities.
	seeds := []factForLLM{
		{File: "g.md", Domain: []string{"auth"}, Entities: []string{}},
		{File: "h.md", Domain: []string{"auth"}, Entities: []string{}},
	}
	clusters := ClusterResult{
		Clusters: map[int][]string{
			0: {"g.md"},
			1: {"h.md"},
		},
	}
	g := store.NewSimilarityGraph([][2]string{{"g.md", "h.md"}})

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.InAnyOrder([]string{"g.md", "h.md"})).Return(g, nil)
	emptyRevdeps := map[string]struct{}{}
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "g.md").Return(emptyRevdeps, nil).AnyTimes()
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "h.md").Return(emptyRevdeps, nil).AnyTimes()

	scope := ScopeFilter{Domain: []string{"auth"}} // "auth" is the scope

	got, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, scope, EffortHigh, filteredCfg,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 bridge (scope token excluded but bridge kept), got %d: %+v", len(got), got)
	}
	if got[0].Token != "" {
		t.Fatalf("want Token==\"\" (scope token cleared), got %q", got[0].Token)
	}
}

// TestBuildFilteredBridges_Determinism verifies that two calls with the same
// inputs produce identical results.
func TestBuildFilteredBridges_Determinism(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	seeds, clusters, g := buildTwoClusterSeeds()
	allPaths := []string{"a.md", "b.md", "c.md", "d.md"}
	emptyRevdeps := map[string]struct{}{}

	idx := NewMockSearchIndex(ctrl)
	// Two calls to buildFilteredBridges → each calls SimilarityAdjacency once.
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.InAnyOrder(allPaths)).Return(g, nil).Times(2)
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).Return(emptyRevdeps, nil).AnyTimes()

	scope := ScopeFilter{}

	run := func() []BridgeSeedSet {
		got, err := buildFilteredBridges(
			context.Background(), idx, "main", seeds, clusters, scope, EffortHigh, filteredCfg,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return got
	}

	r1, r2 := run(), run()
	if len(r1) != len(r2) {
		t.Fatalf("non-deterministic length: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].Token != r2[i].Token {
			t.Fatalf("result[%d].Token non-deterministic: %q vs %q", i, r1[i].Token, r2[i].Token)
		}
		if len(r1[i].Members) != len(r2[i].Members) {
			t.Fatalf("result[%d].Members length non-deterministic", i)
		}
		for j := range r1[i].Members {
			if r1[i].Members[j].File != r2[i].Members[j].File {
				t.Fatalf("result[%d].Members[%d] non-deterministic: %q vs %q", i, j, r1[i].Members[j].File, r2[i].Members[j].File)
			}
		}
	}
}

// TestBuildFilteredBridges_SimilarityAdjacencyError verifies that an error
// from SimilarityAdjacency is propagated to the caller.
func TestBuildFilteredBridges_SimilarityAdjacencyError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	seeds, clusters, _ := buildTwoClusterSeeds()
	wantErr := errors.New("adjacency failed")

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).Return(store.SimilarityGraph{}, wantErr)

	_, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, ScopeFilter{}, EffortHigh, filteredCfg,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("want adjacency error propagated, got %v", err)
	}
}

// TestBuildFilteredBridges_DerivationGapError verifies that an error from
// ReverseDependentPaths (used by derivationGap) is propagated.
func TestBuildFilteredBridges_DerivationGapError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	seeds := []factForLLM{
		{File: "x.md", Domain: []string{"llm"}, Entities: []string{}},
		{File: "y.md", Domain: []string{"llm"}, Entities: []string{}},
	}
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"x.md"}, 1: {"y.md"}},
	}
	g := store.NewSimilarityGraph([][2]string{{"x.md", "y.md"}})
	wantErr := errors.New("revdeps failed")

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).Return(g, nil)
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).Return(nil, wantErr).AnyTimes()

	_, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, ScopeFilter{}, EffortHigh, filteredCfg,
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("want derivationGap error propagated, got %v", err)
	}
}

// TestBuildFilteredBridges_NoCrossCommunitySeam_NilResult verifies that a
// pool where all paths are in the same community (no cross-community seam) →
// no bridges emitted, nil result.
func TestBuildFilteredBridges_NoCrossCommunitySeam_NilResult(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// All four facts in community 0 — same cluster, no cross-community seam.
	seeds := []factForLLM{
		{File: "p.md", Domain: []string{"store"}, Entities: []string{}},
		{File: "q.md", Domain: []string{"store"}, Entities: []string{}},
		{File: "r.md", Domain: []string{"store"}, Entities: []string{}},
		{File: "s.md", Domain: []string{"store"}, Entities: []string{}},
	}
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"p.md", "q.md", "r.md", "s.md"}},
	}
	// Connect everything — but all same-community so no bridge seam.
	g := store.NewSimilarityGraph([][2]string{
		{"p.md", "q.md"}, {"p.md", "r.md"}, {"p.md", "s.md"},
		{"q.md", "r.md"}, {"q.md", "s.md"}, {"r.md", "s.md"},
	})

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().SimilarityAdjacency(gomock.Any(), gomock.Any()).Return(g, nil)
	// No ReverseDependentPaths expected since no subset passes reshape.

	got, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, ScopeFilter{}, EffortHigh, filteredCfg,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty result for same-community pool, got %d bridges", len(got))
	}
}

// TestBuildFilteredBridges_DiscoveredOriginExcluded verifies that seeds with
// Origin==fact.Discovered are excluded from the pool (§7 idempotency), and
// if fewer than 2 non-discovered paths remain the function returns nil.
func TestBuildFilteredBridges_DiscoveredOriginExcluded(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	// No idx calls expected because the pool has < 2 non-discovered facts.

	seeds := []factForLLM{
		{File: "disc1.md", Origin: string(fact.Discovered), Domain: []string{"auth"}},
		{File: "disc2.md", Origin: string(fact.Discovered), Domain: []string{"auth"}},
		{File: "real.md", Domain: []string{"auth"}},
	}
	clusters := ClusterResult{
		Clusters: map[int][]string{0: {"disc1.md"}, 1: {"disc2.md"}, 2: {"real.md"}},
	}

	got, err := buildFilteredBridges(
		context.Background(), idx, "main", seeds, clusters, ScopeFilter{}, EffortHigh, filteredCfg,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil when < 2 non-discovered facts, got %v", got)
	}
}

// pathSetsEqual reports whether two path slices contain the same paths
// (order-independent).
func pathSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]int, len(a))
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
		if m[s] < 0 {
			return false
		}
	}
	return true
}
