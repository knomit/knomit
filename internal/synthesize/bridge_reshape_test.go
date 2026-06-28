package synthesize

import (
	"reflect"
	"testing"

	"knomit/internal/store"
)

// TestReshapeCohesiveSubset_BroadSetWithEmbeddedClique verifies that when
// members contain a cohesive cross-community near-clique embedded in a large
// incoherent remainder, reshapeCohesiveSubset returns exactly the near-clique.
func TestReshapeCohesiveSubset_BroadSetWithEmbeddedClique(t *testing.T) {
	// Communities: clique members are split across two communities (0 and 1).
	// The incoherent "noise" members are in community 2 or absent.
	// Clique: a0..a3 (community 0) + b0..b3 (community 1), fully connected
	// within the clique, no connections to noise members.
	clique := []string{"a0", "a1", "a2", "a3", "b0", "b1", "b2", "b3"}
	noise := []string{"n0", "n1", "n2", "n3", "n4"}

	// Build edges: all clique pairs connected; noise members have no edges.
	var pairs [][2]string
	for i := 0; i < len(clique); i++ {
		for j := i + 1; j < len(clique); j++ {
			pairs = append(pairs, [2]string{clique[i], clique[j]})
		}
	}
	g := store.NewSimilarityGraph(pairs)

	clusterOf := map[string]int{
		"a0": 0, "a1": 0, "a2": 0, "a3": 0,
		"b0": 1, "b1": 1, "b2": 1, "b3": 1,
		"n0": 2, "n1": 2, "n2": 2, "n3": 2, "n4": 2,
	}

	members := append(clique, noise...)

	// maxMembers=8 so entire clique can fit; cohFloor=0.5 (noise adds no edges)
	result := reshapeCohesiveSubset(members, g, clusterOf, 0.5, 8)

	if result == nil {
		t.Fatal("expected non-nil result for embedded clique, got nil")
	}
	if len(result) < 2 {
		t.Fatalf("expected at least 2 members, got %d", len(result))
	}
	if len(result) > 8 {
		t.Fatalf("result exceeds maxMembers=8, got %d", len(result))
	}
	// All returned members must be from the clique (no noise).
	cliqueSet := make(map[string]bool, len(clique))
	for _, c := range clique {
		cliqueSet[c] = true
	}
	for _, r := range result {
		if !cliqueSet[r] {
			t.Errorf("result contains noise member %q — expected only clique members", r)
		}
	}
	// Result must span at least 2 communities (cross-community invariant).
	commA := false
	commB := false
	for _, r := range result {
		if clusterOf[r] == 0 {
			commA = true
		}
		if clusterOf[r] == 1 {
			commB = true
		}
	}
	if !commA || !commB {
		t.Error("result does not span both communities 0 and 1")
	}
}

// TestReshapeCohesiveSubset_IntraCommunityOnlyReturnsNil is THE KEY TEST.
// The only dense neighbourhood is intra-community. A naive densest-subset
// algorithm would return the intra-community clique. The cross-community seed
// requirement must force nil.
func TestReshapeCohesiveSubset_IntraCommunityOnlyReturnsNil(t *testing.T) {
	// Community 0: a0..a3 fully connected (dense).
	// Community 1: b0, b1 connected to each other only.
	// NO cross-community edges.
	intra := []string{"a0", "a1", "a2", "a3"}
	other := []string{"b0", "b1"}

	var pairs [][2]string
	// Fully connect the intra-community clique.
	for i := 0; i < len(intra); i++ {
		for j := i + 1; j < len(intra); j++ {
			pairs = append(pairs, [2]string{intra[i], intra[j]})
		}
	}
	// Connect b0-b1 within community 1.
	pairs = append(pairs, [2]string{"b0", "b1"})
	// NO edges between community 0 and community 1.
	g := store.NewSimilarityGraph(pairs)

	clusterOf := map[string]int{
		"a0": 0, "a1": 0, "a2": 0, "a3": 0,
		"b0": 1, "b1": 1,
	}
	members := append(intra, other...)

	// A naive densest-subset would pick the intra-community clique {a0..a3}.
	// The cross-community requirement must produce nil.
	result := reshapeCohesiveSubset(members, g, clusterOf, 0.5, 6)
	if result != nil {
		t.Errorf("expected nil (no cross-community connected pairs), got %v", result)
	}
}

// TestReshapeCohesiveSubset_NoConnectedCrossCommunityPair returns nil when no
// cross-community pair at all is connected.
func TestReshapeCohesiveSubset_NoConnectedCrossCommunityPair(t *testing.T) {
	// Two communities, members connected only within communities.
	// Same as above but even sparser: only a0-a1 connected within community 0.
	pairs := [][2]string{{"a0", "a1"}}
	g := store.NewSimilarityGraph(pairs)
	clusterOf := map[string]int{"a0": 0, "a1": 0, "b0": 1, "b1": 1}
	members := []string{"a0", "a1", "b0", "b1"}

	result := reshapeCohesiveSubset(members, g, clusterOf, 0.0, 4)
	if result != nil {
		t.Errorf("expected nil (only intra-community edges), got %v", result)
	}
}

// TestReshapeCohesiveSubset_Determinism verifies that two calls with the same
// logical inputs but different member-slice orderings produce byte-equal results.
func TestReshapeCohesiveSubset_Determinism(t *testing.T) {
	// Build a moderately complex graph: two communities with several cross edges.
	// Community 0: x0, x1, x2; Community 1: y0, y1, y2.
	// Cross edges: x0-y0, x0-y1, x1-y0, x1-y1, x2-y2.
	// Intra: x0-x1, x0-x2, x1-x2, y0-y1.
	pairs := [][2]string{
		{"x0", "x1"}, {"x0", "x2"}, {"x1", "x2"},
		{"y0", "y1"},
		{"x0", "y0"}, {"x0", "y1"}, {"x1", "y0"}, {"x1", "y1"}, {"x2", "y2"},
	}
	g := store.NewSimilarityGraph(pairs)
	clusterOf := map[string]int{
		"x0": 0, "x1": 0, "x2": 0,
		"y0": 1, "y1": 1, "y2": 1,
	}

	members1 := []string{"x0", "x1", "x2", "y0", "y1", "y2"}
	members2 := []string{"y2", "y1", "y0", "x2", "x1", "x0"}
	members3 := []string{"x2", "y0", "x0", "y2", "x1", "y1"}

	r1 := reshapeCohesiveSubset(members1, g, clusterOf, 0.4, 6)
	r2 := reshapeCohesiveSubset(members2, g, clusterOf, 0.4, 6)
	r3 := reshapeCohesiveSubset(members3, g, clusterOf, 0.4, 6)

	if r1 == nil {
		t.Fatal("expected non-nil result for determinism test")
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Errorf("result differs between orderings 1 and 2:\n  r1=%v\n  r2=%v", r1, r2)
	}
	if !reflect.DeepEqual(r1, r3) {
		t.Errorf("result differs between orderings 1 and 3:\n  r1=%v\n  r3=%v", r1, r3)
	}
}

// TestReshapeCohesiveSubset_CohFloorTooHigh verifies that when no superset
// beyond the seed pair meets cohFloor, we return the seed pair (density=1.0).
func TestReshapeCohesiveSubset_CohFloorTooHigh(t *testing.T) {
	// Seed pair is cross-community and connected (density = 1.0 for size 2).
	// A third member only connects to one of them (would drop density below 1.0).
	// cohFloor = 0.9 → growth stops after seed.
	pairs := [][2]string{
		{"a", "b"},  // cross-community edge (seed)
		{"b", "c"},  // c connects to b only (would create density 2/3 ≈ 0.67)
	}
	g := store.NewSimilarityGraph(pairs)
	clusterOf := map[string]int{"a": 0, "b": 1}
	// c is absent from clusterOf → its own community; not a problem for this test.
	members := []string{"a", "b", "c"}

	result := reshapeCohesiveSubset(members, g, clusterOf, 0.9, 5)
	if result == nil {
		t.Fatal("expected non-nil result (seed pair a-b is valid)")
	}
	// The seed pair has density 1.0 >= 0.9; adding c drops to 0.67 < 0.9 → stop.
	if len(result) != 2 {
		t.Errorf("expected 2 members (seed pair only), got %d: %v", len(result), result)
	}
	// Must be sorted.
	if result[0] != "a" || result[1] != "b" {
		t.Errorf("expected [a b], got %v", result)
	}
}

// TestReshapeCohesiveSubset_AbsentPathsAreCrossCommunity verifies that two
// members absent from clusterOf are treated as DIFFERENT communities, making
// them cross-community (and thus eligible as a seed pair if connected).
func TestReshapeCohesiveSubset_AbsentPathsAreCrossCommunity(t *testing.T) {
	// "p" and "q" are connected but absent from clusterOf.
	// Per the spec: absent paths are each their own community → p != q → cross-community.
	pairs := [][2]string{{"p", "q"}}
	g := store.NewSimilarityGraph(pairs)
	clusterOf := map[string]int{} // intentionally empty

	result := reshapeCohesiveSubset([]string{"p", "q"}, g, clusterOf, 0.0, 4)
	if result == nil {
		t.Fatal("expected non-nil: connected absent paths are cross-community")
	}
	if len(result) != 2 {
		t.Errorf("expected 2 members, got %d: %v", len(result), result)
	}
}

// TestReshapeCohesiveSubset_ResultIsSorted verifies the output is always
// path-sorted regardless of input ordering.
func TestReshapeCohesiveSubset_ResultIsSorted(t *testing.T) {
	pairs := [][2]string{
		{"z_path", "a_path"},
		{"z_path", "m_path"},
		{"a_path", "m_path"},
	}
	g := store.NewSimilarityGraph(pairs)
	clusterOf := map[string]int{"z_path": 0, "a_path": 1}
	// m_path absent — its own community.
	members := []string{"z_path", "m_path", "a_path"}

	result := reshapeCohesiveSubset(members, g, clusterOf, 0.5, 3)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	for i := 1; i < len(result); i++ {
		if result[i-1] > result[i] {
			t.Errorf("result not sorted at index %d: %v", i, result)
		}
	}
}
