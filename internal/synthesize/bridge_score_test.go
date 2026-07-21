package synthesize

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/mock/gomock"

	"knomit/internal/store"
)

// --- separation tests ---

// TestSeparation_AllKnown_DistinctCommunities verifies that members whose
// community ids are all different produce a separation count equal to the
// number of members.
func TestSeparation_AllKnown_DistinctCommunities(t *testing.T) {
	clusterOf := map[string]int{"a.md": 0, "b.md": 1, "c.md": 2}
	got := separation([]string{"a.md", "b.md", "c.md"}, clusterOf)
	if got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
}

// TestSeparation_AllKnown_SameCommunity verifies that members whose community
// ids are all the same produce a separation count of 1.
func TestSeparation_AllKnown_SameCommunity(t *testing.T) {
	clusterOf := map[string]int{"a.md": 5, "b.md": 5, "c.md": 5}
	got := separation([]string{"a.md", "b.md", "c.md"}, clusterOf)
	if got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
}

// TestSeparation_PartiallyKnown_UnknownAsOwnCommunity verifies that an absent
// member counts as its OWN distinct community (not as community 0 or any real id).
func TestSeparation_PartiallyKnown_UnknownAsOwnCommunity(t *testing.T) {
	// "a.md" and "b.md" share community 0; "unknown.md" is absent.
	// Expected: community 0 (both a+b) + own community for unknown.md = 2.
	clusterOf := map[string]int{"a.md": 0, "b.md": 0}
	got := separation([]string{"a.md", "b.md", "unknown.md"}, clusterOf)
	if got != 2 {
		t.Fatalf("want 2, got %d", got)
	}
}

// TestSeparation_TwoUnknown_AreDistinct verifies that two absent members each
// count as THEIR OWN distinct community — i.e. two unknowns contribute 2, not 1.
func TestSeparation_TwoUnknown_AreDistinct(t *testing.T) {
	clusterOf := map[string]int{"a.md": 0}
	// "b.md" and "c.md" are both absent; they should be two distinct communities.
	got := separation([]string{"a.md", "b.md", "c.md"}, clusterOf)
	if got != 3 {
		t.Fatalf("want 3, got %d", got)
	}
}

// TestSeparation_UnknownCommunityIdZero_NoCollision verifies that an absent
// member does NOT collide with a real member whose community id is 0.
func TestSeparation_UnknownCommunityIdZero_NoCollision(t *testing.T) {
	// "a.md" is in community 0. "b.md" is absent.
	// Without the sentinel trick, both could map to 0 and be counted as one.
	clusterOf := map[string]int{"a.md": 0}
	got := separation([]string{"a.md", "b.md"}, clusterOf)
	if got != 2 {
		t.Fatalf("want 2 (real community 0 + own community for b.md), got %d", got)
	}
}

// TestSeparation_Empty returns 0 for an empty members slice.
func TestSeparation_Empty(t *testing.T) {
	got := separation(nil, map[string]int{})
	if got != 0 {
		t.Fatalf("want 0, got %d", got)
	}
}

// TestSeparation_Single returns 1 for a single member.
func TestSeparation_Single(t *testing.T) {
	got := separation([]string{"a.md"}, map[string]int{"a.md": 7})
	if got != 1 {
		t.Fatalf("want 1, got %d", got)
	}
}

// --- bridgeQ tests ---

// TestBridgeQ_RejectsCohBelowFloor verifies that cohesion below CohFloor
// gates out and returns (0, false).
func TestBridgeQ_RejectsCohBelowFloor(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	c := BridgeComponents{Coh: 0.3, Sep: 2, Gap: 0.5, Spec: 0.5, Members: 3}
	q, ok := bridgeQ(c, cfg)
	if ok {
		t.Fatal("want ok=false when Coh < CohFloor")
	}
	if q != 0 {
		t.Fatalf("want q=0 when gated, got %v", q)
	}
}

// TestBridgeQ_RejectsSepLessThan2 verifies that Sep < 2 gates out (bridges
// must span at least two communities).
func TestBridgeQ_RejectsSepLessThan2(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	c := BridgeComponents{Coh: 0.7, Sep: 1, Gap: 0.5, Spec: 0.5, Members: 3}
	q, ok := bridgeQ(c, cfg)
	if ok {
		t.Fatal("want ok=false when Sep < 2")
	}
	if q != 0 {
		t.Fatalf("want q=0 when gated, got %v", q)
	}
}

// TestBridgeQ_RejectsMembersOverCap verifies that Members > MaxMembers gates
// out.
func TestBridgeQ_RejectsMembersOverCap(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	c := BridgeComponents{Coh: 0.7, Sep: 2, Gap: 0.5, Spec: 0.5, Members: 6}
	q, ok := bridgeQ(c, cfg)
	if ok {
		t.Fatal("want ok=false when Members > MaxMembers")
	}
	if q != 0 {
		t.Fatalf("want q=0 when gated, got %v", q)
	}
}

// TestBridgeQ_PassesGateAndComputesWeightedSum verifies the weighted-sum
// formula when all gates pass and q >= QualityFloor.
func TestBridgeQ_PassesGateAndComputesWeightedSum(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 0.0, WCoh: 1.0, WGap: 2.0, WSpec: 0.5}
	c := BridgeComponents{Coh: 0.6, Sep: 3, Gap: 0.4, Spec: 0.8, Members: 4}
	// q = 1.0*0.6 + 2.0*0.4 + 0.5*0.8 = 0.6 + 0.8 + 0.4 = 1.8
	want := 1.0*0.6 + 2.0*0.4 + 0.5*0.8
	q, ok := bridgeQ(c, cfg)
	if !ok {
		t.Fatal("want ok=true when all gates pass and q >= QualityFloor")
	}
	const epsilon = 1e-9
	if diff := q - want; diff > epsilon || diff < -epsilon {
		t.Fatalf("want q=%v, got %v", want, q)
	}
}

// TestBridgeQ_BelowQualityFloor verifies that a passing gate but q <
// QualityFloor returns (q, false) — note q is still returned, not zeroed.
func TestBridgeQ_BelowQualityFloor(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 5.0, WCoh: 1.0, WGap: 1.0, WSpec: 1.0}
	c := BridgeComponents{Coh: 0.6, Sep: 2, Gap: 0.4, Spec: 0.4, Members: 3}
	// q = 0.6 + 0.4 + 0.4 = 1.4, which is < 5.0 (QualityFloor)
	q, ok := bridgeQ(c, cfg)
	if ok {
		t.Fatal("want ok=false when q < QualityFloor")
	}
	want := 0.6 + 0.4 + 0.4
	const epsilon = 1e-9
	if diff := q - want; diff > epsilon || diff < -epsilon {
		t.Fatalf("want q=%v (non-zero; quality computed but filtered), got %v", want, q)
	}
}

// TestBridgeQ_ExactCohFloor verifies that Coh == CohFloor passes (the gate is
// strictly less-than).
func TestBridgeQ_ExactCohFloor(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	c := BridgeComponents{Coh: 0.5, Sep: 2, Gap: 0.0, Spec: 0.0, Members: 3}
	_, ok := bridgeQ(c, cfg)
	if !ok {
		t.Fatal("want ok=true when Coh == CohFloor (gate is strict <, not <=)")
	}
}

// TestBridgeQ_ExactMaxMembers verifies that Members == MaxMembers passes.
func TestBridgeQ_ExactMaxMembers(t *testing.T) {
	cfg := QualityConfig{CohFloor: 0.5, MaxMembers: 5, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}
	c := BridgeComponents{Coh: 0.6, Sep: 2, Gap: 0.0, Spec: 0.0, Members: 5}
	_, ok := bridgeQ(c, cfg)
	if !ok {
		t.Fatal("want ok=true when Members == MaxMembers (gate is strict >, not >=)")
	}
}

// --- cohesion tests ---

// TestCohesion_DelegatesToDensity verifies that cohesion is a thin wrapper
// over SimilarityGraph.Density. We build a tiny graph via the exported
// zero-value struct (empty adjacency → density 0 for any members).
func TestCohesion_DelegatesToDensity(t *testing.T) {
	// A zero-value SimilarityGraph has no edges → Density returns 0 for any members.
	var g store.SimilarityGraph
	got := cohesion([]string{"a.md", "b.md", "c.md"}, g)
	wantDensity := g.Density([]string{"a.md", "b.md", "c.md"})
	if got != wantDensity {
		t.Fatalf("cohesion() = %v, want %v (g.Density)", got, wantDensity)
	}
}

// --- derivationGap tests ---

// TestDerivationGap_ThreeMembers_OnePairLinked sets up three members where
// exactly one pair is linked (b→a is a reverse dep). The other two pairs
// (a,c) and (b,c) are unlinked → gap = 2/3.
//
// Memoization proof: ReverseDependentPaths must be called exactly once per
// distinct member path (3 calls total), not once per pair.
func TestDerivationGap_ThreeMembers_OnePairLinked(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)

	// revdeps("a.md") = {b.md} → pair (a,b) is linked (b ∈ revdeps(a)).
	// revdeps("b.md") = {} → no additional links from b's side.
	// revdeps("c.md") = {} → c links to nobody.
	// Pairs: (a,b)=linked, (a,c)=unlinked, (b,c)=unlinked → 2 unlinked / 3 total = 2/3.
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "a.md").
		Return(map[string]struct{}{"b.md": {}}, nil).Times(1)
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "b.md").
		Return(map[string]struct{}{}, nil).Times(1)
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), "c.md").
		Return(map[string]struct{}{}, nil).Times(1)

	members := []string{"a.md", "b.md", "c.md"}
	got, err := derivationGap(context.Background(), members, idx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 2.0 / 3.0
	const epsilon = 1e-9
	if diff := got - want; diff > epsilon || diff < -epsilon {
		t.Fatalf("derivationGap = %v, want %v", got, want)
	}
}

// TestDerivationGap_LessThanTwoMembers verifies that fewer than 2 members
// returns 0, nil without calling the index.
func TestDerivationGap_LessThanTwoMembers(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	// No expectations set — any call to idx would fail the test.

	got, err := derivationGap(context.Background(), []string{"a.md"}, idx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("want 0 for <2 members, got %v", got)
	}

	got2, err2 := derivationGap(context.Background(), nil, idx)
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if got2 != 0 {
		t.Fatalf("want 0 for nil members, got %v", got2)
	}
}

// TestDerivationGap_ErrorPropagation verifies the first error from
// ReverseDependentPaths is returned immediately.
func TestDerivationGap_ErrorPropagation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	boom := errors.New("index unavailable")
	// At least one call will error; use AnyTimes so order of member iteration
	// doesn't matter (implementation fetches in slice order, first error wins).
	idx.EXPECT().ReverseDependentPaths(gomock.Any(), gomock.Any()).
		Return(nil, boom).AnyTimes()

	_, err := derivationGap(context.Background(), []string{"x.md", "y.md"}, idx)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom error, got %v", err)
	}
}

// --- specificity tests ---

// TestSpecificity_DF1_ReturnsOne verifies df=1 → spec=1.0 (maximally specific).
func TestSpecificity_DF1_ReturnsOne(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", "auth", "entity").Return(1, nil).Times(1)

	got, err := specificity(context.Background(), "main", "auth", "entity", idx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 1.0
	const epsilon = 1e-9
	if diff := got - want; diff > epsilon || diff < -epsilon {
		t.Fatalf("specificity(df=1) = %v, want %v", got, want)
	}
}

// TestSpecificity_DF4_Returns0_25 verifies df=4 → spec=0.25.
func TestSpecificity_DF4_Returns0_25(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", "cache", "domain").Return(4, nil).Times(1)

	got, err := specificity(context.Background(), "main", "cache", "domain", idx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 0.25
	const epsilon = 1e-9
	if diff := got - want; diff > epsilon || diff < -epsilon {
		t.Fatalf("specificity(df=4) = %v, want %v", got, want)
	}
}

// TestSpecificity_DF0_ReturnsOne verifies df=0 is treated as 1 via max(df,1),
// yielding spec=1.0.
func TestSpecificity_DF0_ReturnsOne(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	idx.EXPECT().TokenDF(gomock.Any(), "main", "rare", "entity").Return(0, nil).Times(1)

	got, err := specificity(context.Background(), "main", "rare", "entity", idx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 1.0
	const epsilon = 1e-9
	if diff := got - want; diff > epsilon || diff < -epsilon {
		t.Fatalf("specificity(df=0) = %v, want %v (df=0 → max(0,1)=1)", got, want)
	}
}

// TestSpecificity_ErrorPropagation verifies TokenDF errors are returned as-is.
func TestSpecificity_ErrorPropagation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	boom := errors.New("token df unavailable")
	idx.EXPECT().TokenDF(gomock.Any(), "main", "tok", "domain").Return(0, boom).Times(1)

	_, err := specificity(context.Background(), "main", "tok", "domain", idx)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom error, got %v", err)
	}
}

// --- scoreBridgeCandidate tests ---

// TestScoreBridgeCandidate_CrossCommunity_Kept verifies that a cohesive
// cross-community set produces Kept=true with the expected components.
//
// Setup: two paths in communities 0 and 1, one SIMILAR_TO edge → Coh=1.0,
// Sep=2, no derivation links → Gap=1.0, df=2 → Spec=0.5, Members=2.
// With cfg WCoh=1, WGap=1, WSpec=0.5 and CohFloor=0.5:
//
//	Q = 1*1.0 + 1*1.0 + 0.5*0.5 = 2.25, Kept=true.
func TestScoreBridgeCandidate_CrossCommunity_Kept(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	paths := []string{"kb/a.md", "kb/b.md"}
	clusterOf := map[string]int{"kb/a.md": 0, "kb/b.md": 1}
	g := store.NewSimilarityGraph([][2]string{{"kb/a.md", "kb/b.md"}})

	// No derivation links between members → Gap = 1.0.
	idx.EXPECT().ReverseDependentPaths(ctx, "kb/a.md").
		Return(map[string]struct{}{}, nil).Times(1)
	idx.EXPECT().ReverseDependentPaths(ctx, "kb/b.md").
		Return(map[string]struct{}{}, nil).Times(1)
	// TokenDF for entity kind (BridgeEntity → token used as-is).
	idx.EXPECT().TokenDF(ctx, "branch/test", "bridgeTok", string(BridgeEntity)).
		Return(2, nil).Times(1)

	cfg := QualityConfig{
		CohFloor:     0.5,
		QualityFloor: 0.0,
		WCoh:         1.0,
		WGap:         1.0,
		WSpec:        0.5,
		MaxMembers:   10,
	}

	comp, q, kept, err := scoreBridgeCandidate(ctx, paths, BridgeEntity, "bridgeTok", g, idx, "branch/test", clusterOf, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert components.
	const epsilon = 1e-9
	if comp.Coh < 1.0-epsilon || comp.Coh > 1.0+epsilon {
		t.Errorf("Coh = %v, want 1.0 (one pair, one edge → density 1.0)", comp.Coh)
	}
	if comp.Sep != 2 {
		t.Errorf("Sep = %d, want 2 (two different communities)", comp.Sep)
	}
	if comp.Members != 2 {
		t.Errorf("Members = %d, want 2", comp.Members)
	}
	if !kept {
		t.Error("want Kept=true for cohesive cross-community set")
	}
	if q <= 0 {
		t.Errorf("want Q>0, got %v", q)
	}
	// Q = 1*1.0 + 1*1.0 + 0.5*0.5 = 2.25
	wantQ := 1.0*1.0 + 1.0*1.0 + 0.5*0.5
	if diff := q - wantQ; diff > epsilon || diff < -epsilon {
		t.Errorf("Q = %v, want %v", q, wantQ)
	}
}

// TestScoreBridgeCandidate_ErrorPropagation_DerivationGap verifies that an
// error from ReverseDependentPaths (inside derivationGap) is returned by
// scoreBridgeCandidate.
func TestScoreBridgeCandidate_ErrorPropagation_DerivationGap(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	paths := []string{"kb/x.md", "kb/y.md"}
	clusterOf := map[string]int{"kb/x.md": 0, "kb/y.md": 1}
	g := store.NewSimilarityGraph([][2]string{{"kb/x.md", "kb/y.md"}})

	boom := errors.New("revdeps unavailable")
	idx.EXPECT().ReverseDependentPaths(ctx, gomock.Any()).
		Return(nil, boom).AnyTimes()

	cfg := QualityConfig{CohFloor: 0.0, MaxMembers: 10, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}

	_, _, _, err := scoreBridgeCandidate(ctx, paths, BridgeEntity, "tok", g, idx, "branch/test", clusterOf, cfg)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom error from derivationGap propagation, got %v", err)
	}
}

// TestScoreBridgeCandidate_ErrorPropagation_Specificity verifies that an error
// from TokenDF (inside specificity) is propagated by scoreBridgeCandidate.
func TestScoreBridgeCandidate_ErrorPropagation_Specificity(t *testing.T) {
	ctx := context.Background()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	idx := NewMockSearchIndex(ctrl)
	// Only one member → derivationGap returns (0, nil) without calling idx.
	paths := []string{"kb/z.md"}
	clusterOf := map[string]int{"kb/z.md": 0}
	g := store.NewSimilarityGraph(nil)

	boom := errors.New("tokendf unavailable")
	idx.EXPECT().TokenDF(ctx, gomock.Any(), gomock.Any(), gomock.Any()).
		Return(0, boom).Times(1)

	cfg := QualityConfig{CohFloor: 0.0, MaxMembers: 10, QualityFloor: 0.0, WCoh: 1, WGap: 1, WSpec: 1}

	_, _, _, err := scoreBridgeCandidate(ctx, paths, BridgeEntity, "tok", g, idx, "branch/test", clusterOf, cfg)
	if !errors.Is(err, boom) {
		t.Fatalf("want boom error from specificity propagation, got %v", err)
	}
}
