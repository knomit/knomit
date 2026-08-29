package embeddings

// MinBatchTokens is the smallest budget worth configuring: one full-length
// document. Below MaxTokens (2048) a single max-length document cannot fit in
// the budget at all, so every long document runs alone via packByTokenBudget's
// singleton exception and the budget stops bounding anything. A machine at this
// floor still works — one 2048-token row measured 225 MiB — but it should be
// logged, because it means the host is small enough to change how knomit runs.
const MinBatchTokens = 2048

// ResidentModelBytes is the measured resident footprint of the loaded
// embeddinggemma model and tokenizer, before any batch runs: ~675 MiB, stable
// across every measured configuration (observed range 670-686 MiB). Memory
// available for BATCHES is whatever remains after this.
const ResidentModelBytes = 675 << 20

// budgetLadder is the measured budget -> worst-case-batch-delta curve, in bytes
// above ResidentModelBytes.
//
// This IS the worst-case curve rather than a sample of it: for a budget B the
// most expensive shape B permits is B/MaxTokens rows at full 2048 width, and
// each row below was measured at exactly that shape, one process per
// configuration.
//
// It is deliberately a table and not a bytes-per-token constant. Per-token cost
// is NOT constant — measured at ~71 KiB/token at seq=128 rising to ~141 KiB at
// seq=2048, and rising with batch size too — so any single constant is wrong in
// one direction or the other. A constant taken from batch=32/seq=2048
// overestimates (that shape is unreachable under these budgets); one taken from
// the cheap shapes underestimates. Interpolating measured points avoids both.
var budgetLadder = []struct {
	tokens int
	bytes  int64
}{
	{2048, 225 << 20},
	{4096, 448 << 20},
	{8192, 910 << 20},
	{16384, 1820 << 20},
}

// BudgetForBatchMemory returns the largest padded-token budget whose measured
// worst-case batch fits in availBytes, clamped to [MinBatchTokens,
// DefaultMaxBatchTokens].
//
// availBytes is memory available for ONE BATCH — the caller subtracts
// ResidentModelBytes and its own non-embedding footprint first.
//
// The upper clamp is the clamp-DOWN-only rule and is not an implementation
// detail: above DefaultMaxBatchTokens per-run amortization is already
// exhausted, while the ONNX arena's retained high-water mark keeps growing
// linearly, so a large host would pin gigabytes for no measured gain. This
// function can therefore only ever reduce the shipped default, never raise it.
//
// WHAT THIS DOES NOT MEAN: the returned budget bounds ONE session.Run, not the
// process. Concurrent branch work on the shared Embedder overlaps and peaks
// add, so a value derived here is a per-run expectation, not a process ceiling.
func BudgetForBatchMemory(availBytes int64) int {
	if availBytes < budgetLadder[0].bytes {
		return MinBatchTokens
	}

	budget := budgetLadder[0].tokens
	for i := 1; i < len(budgetLadder); i++ {
		lo, hi := budgetLadder[i-1], budgetLadder[i]
		if availBytes >= hi.bytes {
			budget = hi.tokens
			continue
		}
		// Between two measured points: interpolate linearly rather than snapping
		// down to the lower rung, which would waste up to half the headroom.
		span := hi.bytes - lo.bytes
		over := availBytes - lo.bytes
		budget = lo.tokens + int(int64(hi.tokens-lo.tokens)*over/span)
		break
	}

	if budget > DefaultMaxBatchTokens {
		budget = DefaultMaxBatchTokens
	}
	if budget < MinBatchTokens {
		budget = MinBatchTokens
	}
	return budget
}
