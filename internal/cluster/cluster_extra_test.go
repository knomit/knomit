package cluster

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// CosineDistance
// ---------------------------------------------------------------------------

func TestCosineDistance_KnownVectors(t *testing.T) {
	// Two vectors at 60° have cosine similarity 0.5, so distance = 0.5.
	a := []float64{1, 0}
	b := []float64{0.5, math.Sqrt(3) / 2} // unit vector at 60°
	got := CosineDistance(a, b)
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("CosineDistance(60°) = %v, want 0.5", got)
	}
}

func TestCosineDistance_IdenticalVectors(t *testing.T) {
	a := []float64{3, 4, 5}
	got := CosineDistance(a, a)
	if math.Abs(got) > 1e-9 {
		t.Fatalf("CosineDistance(identical) = %v, want 0", got)
	}
}

func TestCosineDistance_OrthogonalVectors(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{0, 1, 0}
	got := CosineDistance(a, b)
	if math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("CosineDistance(orthogonal) = %v, want 1.0", got)
	}
}

func TestCosineDistance_OppositeVectors(t *testing.T) {
	a := []float64{1, 2, 3}
	b := []float64{-1, -2, -3}
	got := CosineDistance(a, b)
	if math.Abs(got-2.0) > 1e-9 {
		t.Fatalf("CosineDistance(opposite) = %v, want 2.0", got)
	}
}

func TestCosineDistance_ZeroVectorA(t *testing.T) {
	a := []float64{0, 0, 0}
	b := []float64{1, 2, 3}
	got := CosineDistance(a, b)
	if got != 1.0 {
		t.Fatalf("CosineDistance(zero, non-zero) = %v, want 1.0", got)
	}
}

func TestCosineDistance_ZeroVectorB(t *testing.T) {
	a := []float64{1, 2, 3}
	b := []float64{0, 0, 0}
	got := CosineDistance(a, b)
	if got != 1.0 {
		t.Fatalf("CosineDistance(non-zero, zero) = %v, want 1.0", got)
	}
}

func TestCosineDistance_BothZero(t *testing.T) {
	a := []float64{0, 0}
	b := []float64{0, 0}
	got := CosineDistance(a, b)
	if got != 1.0 {
		t.Fatalf("CosineDistance(zero, zero) = %v, want 1.0", got)
	}
}

// ---------------------------------------------------------------------------
// findABSlow
// ---------------------------------------------------------------------------

func TestFindABSlow_MinDist01(t *testing.T) {
	// Compare with the precomputed cache for minDist=0.1.
	a, b := findABSlow(0.1)
	wantA, wantB := 1.576636002939383, 0.894641445561886
	if math.Abs(a-wantA) > 0.01 || math.Abs(b-wantB) > 0.01 {
		t.Fatalf("findABSlow(0.1) = (%v, %v), want ~(%v, %v)", a, b, wantA, wantB)
	}
}

func TestFindABSlow_MinDist05(t *testing.T) {
	// a, b should be positive and reasonable.
	a, b := findABSlow(0.5)
	if a <= 0 || b <= 0 {
		t.Fatalf("findABSlow(0.5) returned non-positive: a=%v, b=%v", a, b)
	}
	// For larger minDist, 'a' should be smaller (flatter curve).
	a01, _ := findABSlow(0.1)
	if a >= a01 {
		t.Fatalf("findABSlow(0.5).a=%v should be < findABSlow(0.1).a=%v", a, a01)
	}
}

func TestFindABSlow_MinDist001(t *testing.T) {
	a, b := findABSlow(0.01)
	if a <= 0 || b <= 0 {
		t.Fatalf("findABSlow(0.01) returned non-positive: a=%v, b=%v", a, b)
	}
}

// ---------------------------------------------------------------------------
// findAB (cached + fallback)
// ---------------------------------------------------------------------------

func TestFindAB_CachedPath(t *testing.T) {
	// minDist=0.1 is cached; should return exact precomputed values.
	a, b := findAB(0.1)
	if a != 1.576636002939383 || b != 0.894641445561886 {
		t.Fatalf("findAB(0.1) = (%v, %v), want exact cached values", a, b)
	}
}

func TestFindAB_NonCachedPath(t *testing.T) {
	// minDist=0.2 is NOT cached; should fall through to findABSlow.
	a, b := findAB(0.2)
	if a <= 0 || b <= 0 {
		t.Fatalf("findAB(0.2) returned non-positive: a=%v, b=%v", a, b)
	}
	// Cross-check with direct slow call.
	aSlow, bSlow := findABSlow(0.2)
	if a != aSlow || b != bSlow {
		t.Fatalf("findAB(0.2) = (%v,%v), findABSlow(0.2) = (%v,%v); should match",
			a, b, aSlow, bSlow)
	}
}

// ---------------------------------------------------------------------------
// lambdaOf
// ---------------------------------------------------------------------------

func TestLambdaOf_Zero(t *testing.T) {
	got := lambdaOf(0)
	if !math.IsInf(got, 1) {
		t.Fatalf("lambdaOf(0) = %v, want +Inf", got)
	}
}

func TestLambdaOf_Positive(t *testing.T) {
	got := lambdaOf(0.5)
	if got != 2.0 {
		t.Fatalf("lambdaOf(0.5) = %v, want 2.0", got)
	}
}

func TestLambdaOf_One(t *testing.T) {
	got := lambdaOf(1.0)
	if got != 1.0 {
		t.Fatalf("lambdaOf(1.0) = %v, want 1.0", got)
	}
}

func TestLambdaOf_Large(t *testing.T) {
	got := lambdaOf(1000.0)
	if math.Abs(got-0.001) > 1e-12 {
		t.Fatalf("lambdaOf(1000) = %v, want 0.001", got)
	}
}

// ---------------------------------------------------------------------------
// unionFind — union of already-same-set elements
// ---------------------------------------------------------------------------

func TestUnion_AlreadySameSet(t *testing.T) {
	uf := newUnionFind(5)
	uf.union(0, 1)

	// 0 and 1 are already in the same set; union should be a no-op.
	rankBefore := make([]int, len(uf.rank))
	copy(rankBefore, uf.rank)

	uf.union(0, 1)

	// Verify they are still in the same set.
	if uf.find(0) != uf.find(1) {
		t.Fatalf("after redundant union, 0 and 1 should share a root")
	}

	// Rank should not have changed.
	for i, r := range uf.rank {
		if r != rankBefore[i] {
			t.Fatalf("rank[%d] changed from %d to %d after redundant union", i, rankBefore[i], r)
		}
	}
}

func TestUnion_TransitiveChain(t *testing.T) {
	uf := newUnionFind(6)
	// Build chain: 0-1, 1-2, 2-3, 3-4, 4-5
	for i := 0; i < 5; i++ {
		uf.union(i, i+1)
	}
	root := uf.find(0)
	for i := 1; i < 6; i++ {
		if uf.find(i) != root {
			t.Fatalf("element %d has root %d, want %d", i, uf.find(i), root)
		}
	}

	// Redundant union across the chain should be a no-op.
	uf.union(0, 5)
	if uf.find(0) != uf.find(5) {
		t.Fatalf("after redundant cross-chain union, 0 and 5 should share a root")
	}
}

func TestUnion_DisjointSets(t *testing.T) {
	uf := newUnionFind(4)
	uf.union(0, 1)
	uf.union(2, 3)

	// Confirm two disjoint sets.
	if uf.find(0) == uf.find(2) {
		t.Fatalf("sets {0,1} and {2,3} should be disjoint before merge")
	}

	// Now merge them.
	uf.union(1, 3)
	root := uf.find(0)
	for i := 0; i < 4; i++ {
		if uf.find(i) != root {
			t.Fatalf("after merge, element %d has root %d, want %d", i, uf.find(i), root)
		}
	}
}
