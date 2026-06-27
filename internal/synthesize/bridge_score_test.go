package synthesize

import (
	"testing"

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
