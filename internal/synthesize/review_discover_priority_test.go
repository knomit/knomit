package synthesize

import "testing"

// TestForwardDiscoverPriority_StrictlyNegativeRanked is the regression guard for
// the priority-leak bug. Forward "discover" work items must run AFTER the
// standard prune (priority = cluster size, > 0) and distill (priority 0) items,
// so their priority must stay strictly negative regardless of bridge strength.
//
// The old formula `-10 + b.Strength` fed Strength (== the number of communities
// a bridge token spans) straight into the priority; a token spanning >10
// communities produced a positive priority that leapfrogged prune/distill. The
// fix ranks by position in the strength-sorted slice instead, so priority is a
// function of rank only and never escapes the negative band.
func TestForwardDiscoverPriority_StrictlyNegativeRanked(t *testing.T) {
	// Even a very large discover queue stays strictly below distill (0).
	prev := 0.0
	for rank := 0; rank < 1000; rank++ {
		p := forwardDiscoverPriority(rank)
		if p >= 0 {
			t.Fatalf("forwardDiscoverPriority(%d) = %v, must be strictly negative (below distill's 0)", rank, p)
		}
		if rank > 0 && p >= prev {
			t.Fatalf("priority must strictly decrease with rank: rank %d = %v, prev = %v", rank, p, prev)
		}
		prev = p
	}

	// Highest-strength bridge (rank 0) sits at the top of the discover band.
	if got := forwardDiscoverPriority(0); got != forwardDiscoverPriorityBase {
		t.Errorf("rank 0 priority = %v, want %v (top of discover band)", got, forwardDiscoverPriorityBase)
	}

	// The base must sit below distill (0) and above reflect, matching the
	// intended prune > distill > discover > reflect ordering.
	if forwardDiscoverPriorityBase >= 0 {
		t.Errorf("discover band base %v must be below distill (0)", forwardDiscoverPriorityBase)
	}
	if forwardDiscoverPriorityBase <= reflectPriority {
		t.Errorf("discover band base %v must be above reflect (%d)", forwardDiscoverPriorityBase, reflectPriority)
	}

	// The maxBridgeSeeds cap (bridge.go) is what makes "discover > reflect" hold
	// in practice: the deepest possible discover item is at rank maxBridgeSeeds-1,
	// and its priority must stay strictly above reflect. Without the cap, an
	// unbounded scoped pool would push rank ≥ 90 and collide with reflect's -100.
	deepest := forwardDiscoverPriority(maxBridgeSeeds - 1)
	if deepest <= reflectPriority {
		t.Errorf("deepest capped discover priority %v (rank %d) must stay above reflect %d",
			deepest, maxBridgeSeeds-1, reflectPriority)
	}
}
