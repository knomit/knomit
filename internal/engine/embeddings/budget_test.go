package embeddings

import "testing"

const mib = int64(1) << 20

// TestBudgetForBatchMemory_MatchesMeasuredLadder pins the inversion against the
// measured points themselves. The ladder IS the budget->worst-delta curve: for
// a budget B the worst shape B permits is B/2048 rows at full width, and each
// of these was measured at that shape. Inverting the table beats dividing by a
// KiB/token constant, because per-token cost is not constant — it rises with
// both batch and sequence length, so any single constant is wrong in one
// direction or the other.
func TestBudgetForBatchMemory_MatchesMeasuredLadder(t *testing.T) {
	for _, tc := range []struct {
		availMiB int64
		want     int
	}{
		{225, 2048},
		{448, 4096},
		{910, 8192},
		{1820, 16384},
	} {
		if got := BudgetForBatchMemory(tc.availMiB * mib); got != tc.want {
			t.Errorf("BudgetForBatchMemory(%d MiB) = %d, want %d", tc.availMiB, got, tc.want)
		}
	}
}

// TestBudgetForBatchMemory_NeverSizesUp is the clamp-DOWN-only rule. Above the
// default budget, per-run amortization is exhausted while retained arena memory
// keeps growing — a big host would pin gigabytes for no measured gain.
func TestBudgetForBatchMemory_NeverSizesUp(t *testing.T) {
	for _, availMiB := range []int64{1820, 4000, 64000, 1 << 20} {
		if got := BudgetForBatchMemory(availMiB * mib); got > DefaultMaxBatchTokens {
			t.Errorf("BudgetForBatchMemory(%d MiB) = %d, want <= %d — never size up",
				availMiB, got, DefaultMaxBatchTokens)
		}
	}
}

// TestBudgetForBatchMemory_FloorIsOneFullDocument guards the low end. Below
// MaxTokens a single max-length document cannot fit in the budget at all, so
// every long document would run alone via the singleton exception — the budget
// would stop meaning anything. Tiny, zero and negative headroom all land here.
func TestBudgetForBatchMemory_FloorIsOneFullDocument(t *testing.T) {
	for _, availMiB := range []int64{-1000, 0, 1, 100, 224} {
		if got := BudgetForBatchMemory(availMiB * mib); got != MinBatchTokens {
			t.Errorf("BudgetForBatchMemory(%d MiB) = %d, want the floor %d",
				availMiB, got, MinBatchTokens)
		}
	}
	if MinBatchTokens < 2048 {
		t.Errorf("MinBatchTokens = %d, want >= 2048 (one full-length document)", MinBatchTokens)
	}
}

// TestBudgetForBatchMemory_MonotonicAndInterpolated: more headroom must never
// yield a smaller budget, and values between measured rungs must land between
// those rungs rather than snapping to one.
func TestBudgetForBatchMemory_MonotonicAndInterpolated(t *testing.T) {
	prev := 0
	for availMiB := int64(0); availMiB <= 2200; availMiB += 25 {
		got := BudgetForBatchMemory(availMiB * mib)
		if got < prev {
			t.Fatalf("not monotonic: %d MiB -> %d after %d", availMiB, got, prev)
		}
		prev = got
	}

	mid := BudgetForBatchMemory(679 * mib) // between the 448 and 910 rungs
	if mid <= 4096 || mid >= 8192 {
		t.Errorf("BudgetForBatchMemory(679 MiB) = %d, want strictly between 4096 and 8192", mid)
	}
}

// TestResidentModelBytes_IsNonZero: the app layer subtracts this before asking
// for a budget, so a zero would silently over-budget by the model's footprint.
func TestResidentModelBytes_IsNonZero(t *testing.T) {
	if ResidentModelBytes <= 0 {
		t.Errorf("ResidentModelBytes = %d, want the measured resident footprint", ResidentModelBytes)
	}
}
