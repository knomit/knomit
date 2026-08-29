package embeddings

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
)

// cost is the memory-relevant size of a batch: every row is padded to the
// longest row in it, so the tensor fed to ONNX is rows × longest. Peak RSS
// measured on embeddinggemma tracks this product (~71 KiB/token-row at
// seq=128 rising to ~141 KiB at seq=2048), which is why the budget bounds it
// rather than bounding the document count.
func cost(lens []int, batch []int) int {
	maxLen := 0
	for _, i := range batch {
		if lens[i] > maxLen {
			maxLen = lens[i]
		}
	}
	return len(batch) * maxLen
}

// TestPackByTokenBudget_NeverExceedsBudget is the invariant the whole change
// exists to establish: no single session.Run may be handed more padded tokens
// than the budget. A batch of one is the sole exception — a document cannot be
// split, so an oversized document still runs alone.
func TestPackByTokenBudget_NeverExceedsBudget(t *testing.T) {
	cases := []struct {
		name   string
		lens   []int
		budget int
	}{
		{"uniform long docs", repeatLen(2048, 32), 16384},
		{"uniform short facts", repeatLen(128, 100), 16384},
		{"one long doc among short ones", append([]int{2048}, repeatLen(128, 99)...), 16384},
		{"descending ladder", []int{2048, 1024, 512, 256, 128, 64, 32, 16, 8, 4, 2, 1}, 4096},
		{"ascending ladder", []int{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048}, 4096},
		{"single doc", []int{2048}, 16384},
		{"tiny budget forces singletons", repeatLen(2048, 4), 512},
		{"zero-length rows", []int{0, 0, 5}, 4096},
		{"zero-length rows, zero budget", []int{0, 0, 5}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			batches := packByTokenBudget(tc.lens, tc.budget)
			for bi, b := range batches {
				if len(b) == 0 {
					t.Fatalf("batch %d is empty", bi)
				}
				if c := cost(tc.lens, b); c > tc.budget && len(b) > 1 {
					t.Errorf("batch %d: cost %d exceeds budget %d with %d rows; only a singleton may overflow",
						bi, c, tc.budget, len(b))
				}
			}
		})
	}
}

// TestPackByTokenBudget_CoversEveryRowExactlyOnce guards the scatter/gather:
// a dropped index silently loses a fact's embedding, a duplicated one silently
// overwrites a neighbour's.
func TestPackByTokenBudget_CoversEveryRowExactlyOnce(t *testing.T) {
	lens := []int{2048, 7, 512, 512, 1, 1024, 300, 300, 300, 2048}
	got := []int{}
	for _, b := range packByTokenBudget(lens, 4096) {
		got = append(got, b...)
	}
	sort.Ints(got)
	if len(got) != len(lens) {
		t.Fatalf("packed %d rows, want %d", len(got), len(lens))
	}
	for i := range got {
		if got[i] != i {
			t.Fatalf("packed indices = %v, want each of 0..%d exactly once", got, len(lens)-1)
		}
	}
}

// TestPackByTokenBudget_OversizedRowRunsAlone pins the no-infinite-loop edge:
// a row longer than the entire budget must still be emitted, as a batch of one.
func TestPackByTokenBudget_OversizedRowRunsAlone(t *testing.T) {
	batches := packByTokenBudget([]int{5000, 10}, 1024)
	for _, b := range batches {
		if len(b) == 1 && b[0] == 0 {
			return
		}
	}
	t.Fatalf("oversized row was not emitted as its own batch: %v", batches)
}

// TestPackByTokenBudget_EmptyInput keeps the empty case from producing a
// phantom batch that would hand ONNX a zero-row tensor.
func TestPackByTokenBudget_EmptyInput(t *testing.T) {
	if b := packByTokenBudget(nil, 16384); len(b) != 0 {
		t.Fatalf("packByTokenBudget(nil) = %v, want no batches", b)
	}
}

// TestPackByTokenBudget_ChargesShortRowsAtTheMinimum pins the rail. Rows are
// charged max(len, minRowCharge), which caps a batch at budget/minRowCharge
// rows however short the rows are. That cap does two jobs: it bounds the
// per-row overhead measured at batch=2048/seq=8 (1683 MiB delta vs ~1150 for
// the same token count in fewer rows), and it preserves the cancellation
// contract in store/interfaces.go — without it, learn.go (which embeds an
// entire incoming write request, uncapped) and motif_alias.go (a whole motif
// vocabulary in one call) would each become one uninterruptible session.Run.
func TestPackByTokenBudget_ChargesShortRowsAtTheMinimum(t *testing.T) {
	const budget = 16384
	batches := packByTokenBudget(repeatLen(8, 300), budget)
	if len(batches) == 0 {
		t.Fatal("no batches")
	}
	want := budget / minRowCharge
	if len(batches[0]) != want {
		t.Errorf("first batch has %d rows, want exactly %d (budget/minRowCharge); "+
			"short rows must be charged at the minimum, not their raw length",
			len(batches[0]), want)
	}
}

// TestPackByTokenBudget_CliffCutoff pins the padding-waste guard. Descending
// order keeps neighbours similar, but at a length cliff a long row would
// otherwise drag short rows up to its width — real ONNX compute, not just
// memory. Budget 16384 would otherwise put the 2048-token doc with seven
// 128-token facts, all padded to 2048; the cutoff stops it at one neighbour.
func TestPackByTokenBudget_CliffCutoff(t *testing.T) {
	lens := append([]int{2048}, repeatLen(128, 99)...)
	batches := packByTokenBudget(lens, 16384)
	if len(batches[0]) > 2 {
		t.Errorf("first batch has %d rows, want at most 2 — the long doc should not "+
			"drag a run of 128-token rows up to width 2048", len(batches[0]))
	}
}

// TestPackByTokenBudget_CliffNeverCutsToSingleton guards the cutoff against
// eating the packer it protects. A batch of one wastes no padding by
// definition, so cutting there buys nothing and costs a run. Without the guard
// a smoothly decaying corpus — every neighbour more than 2x smaller — becomes
// one run per document, the exact pathology this packer exists to avoid.
func TestPackByTokenBudget_CliffNeverCutsToSingleton(t *testing.T) {
	t.Run("adjacent pair stays paired", func(t *testing.T) {
		batches := packByTokenBudget([]int{2048, 1000}, 16384)
		if len(batches) != 1 || len(batches[0]) != 2 {
			t.Errorf("got %v, want a single batch of 2 — a cut here saves no padding", batches)
		}
	})

	t.Run("decaying corpus does not become all singletons", func(t *testing.T) {
		lens := []int{2048, 1000, 490, 240, 118}
		batches := packByTokenBudget(lens, 16384)
		if len(batches) >= len(lens) {
			t.Errorf("got %d batches for %d rows (%v) — a smoothly decaying corpus "+
				"degenerated to one run per document", len(batches), len(lens), batches)
		}
	})
}

// TestEmbedInBatches_PreservesInputOrder is the correctness cost of packing:
// batching by length reorders rows, so results must be scattered back to their
// input positions. Getting this wrong assigns every fact its neighbour's
// vector — a corruption no length assertion would catch.
func TestEmbedInBatches_PreservesInputOrder(t *testing.T) {
	// Lengths chosen so the packer definitely reorders: interleaved long/short.
	rows := make([]encodedRow, 12)
	want := make([][]float32, len(rows))
	for i := range rows {
		n := 8
		if i%2 == 0 {
			n = 2000
		}
		rows[i] = encodedRow{ids: make([]int64, n), mask: make([]int64, n)}
		// Tag each row with its input index so we can assert identity.
		rows[i].ids[0] = int64(i)
		want[i] = []float32{float32(i)}
	}

	run := func(batch []encodedRow) ([][]float32, error) {
		out := make([][]float32, len(batch))
		for i, r := range batch {
			out[i] = []float32{float32(r.ids[0])}
		}
		return out, nil
	}

	got, err := embedInBatches(context.Background(), rows, 4096, nil, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vectors, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i][0] != want[i][0] {
			t.Errorf("row %d: got vector tagged %v, want %v — results were not restored to input order",
				i, got[i][0], want[i][0])
		}
	}
}

// TestEmbedInBatches_BoundsEveryRunByBudget asserts the invariant end-to-end
// through the loop rather than in the packer alone.
func TestEmbedInBatches_BoundsEveryRunByBudget(t *testing.T) {
	const budget = 4096
	rows := make([]encodedRow, 40)
	for i := range rows {
		n := 1024
		if i%3 == 0 {
			n = 2048
		}
		rows[i] = encodedRow{ids: make([]int64, n), mask: make([]int64, n)}
	}

	run := func(batch []encodedRow) ([][]float32, error) {
		maxLen := 0
		for _, r := range batch {
			if len(r.ids) > maxLen {
				maxLen = len(r.ids)
			}
		}
		if c := len(batch) * maxLen; c > budget && len(batch) > 1 {
			return nil, fmt.Errorf("run got %d padded tokens (%d rows × %d), over budget %d",
				c, len(batch), maxLen, budget)
		}
		return make([][]float32, len(batch)), nil
	}

	if _, err := embedInBatches(context.Background(), rows, budget, nil, run); err != nil {
		t.Fatal(err)
	}
}

// TestEmbedInBatches_NonPositiveBudgetFallsBackToOneRunPerRow keeps a
// misconfigured budget from wedging the packer or silently restoring the
// unbounded behavior this change removes.
func TestEmbedInBatches_NonPositiveBudgetFallsBackToOneRunPerRow(t *testing.T) {
	rows := []encodedRow{
		{ids: make([]int64, 10), mask: make([]int64, 10)},
		{ids: make([]int64, 20), mask: make([]int64, 20)},
	}
	runs := 0
	run := func(batch []encodedRow) ([][]float32, error) {
		runs++
		if len(batch) != 1 {
			t.Errorf("batch of %d rows on a non-positive budget, want singletons", len(batch))
		}
		return make([][]float32, len(batch)), nil
	}
	if _, err := embedInBatches(context.Background(), rows, 0, nil, run); err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Errorf("ran %d times, want one per row", runs)
	}
}

// TestEmbedInBatches_CancelledBetweenBatches carries forward the per-batch
// cancellation checkpoint across the move from fixed chunks to packed batches.
func TestEmbedInBatches_CancelledBetweenBatches(t *testing.T) {
	rows := make([]encodedRow, 64)
	for i := range rows {
		rows[i] = encodedRow{ids: make([]int64, 2048), mask: make([]int64, 2048)}
	}

	t.Run("cancelled after the first batch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		batches := 0
		run := func(batch []encodedRow) ([][]float32, error) {
			batches++
			cancel()
			return make([][]float32, len(batch)), nil
		}
		if _, err := embedInBatches(ctx, rows, 4096, nil, run); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if batches != 1 {
			t.Errorf("ran %d batches after cancellation, want 1 (the in-flight one)", batches)
		}
	})

	t.Run("pre-cancelled runs no inference at all", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		run := func(batch []encodedRow) ([][]float32, error) {
			t.Fatal("run must not be called on a pre-cancelled context")
			return nil, nil
		}
		if _, err := embedInBatches(ctx, rows, 4096, nil, run); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("empty input still observes cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		run := func(batch []encodedRow) ([][]float32, error) {
			t.Fatal("run must not be called for empty input")
			return nil, nil
		}
		if _, err := embedInBatches(ctx, nil, 4096, nil, run); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func repeatLen(n, count int) []int {
	out := make([]int, count)
	for i := range out {
		out[i] = n
	}
	return out
}

// TestEmbedInBatches_ErrorReturnsNoPartialResults pins the error path. Results
// are scattered into a preallocated slice, so a mid-corpus failure would
// otherwise return a slice that is the right length but silently full of nil
// holes — which a caller checking only len() would accept as success.
func TestEmbedInBatches_ErrorReturnsNoPartialResults(t *testing.T) {
	rows := make([]encodedRow, 40)
	for i := range rows {
		rows[i] = encodedRow{ids: make([]int64, 2048), mask: make([]int64, 2048)}
	}

	calls := 0
	boom := errors.New("inference exploded")
	run := func(batch []encodedRow) ([][]float32, error) {
		calls++
		if calls == 2 {
			return nil, boom
		}
		out := make([][]float32, len(batch))
		for i := range out {
			out[i] = []float32{1}
		}
		return out, nil
	}

	got, err := embedInBatches(context.Background(), rows, 4096, nil, run)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if got != nil {
		t.Errorf("got %d results alongside the error, want nil — a partially "+
			"scattered slice would read as success to a len()-checking caller", len(got))
	}
}

// TestEmbedInBatches_ShortReturnFromRunIsRejected guards the scatter against an
// embedder that breaks the 1:1 contract: writing fewer vectors than rows would
// leave nil holes that callers index positionally (search_index.go, learn.go).
func TestEmbedInBatches_ShortReturnFromRunIsRejected(t *testing.T) {
	rows := make([]encodedRow, 4)
	for i := range rows {
		rows[i] = encodedRow{ids: make([]int64, 100), mask: make([]int64, 100)}
	}
	run := func(batch []encodedRow) ([][]float32, error) {
		return make([][]float32, len(batch)-1), nil // one short
	}
	if _, err := embedInBatches(context.Background(), rows, 16384, nil, run); err == nil {
		t.Fatal("short return from run was accepted, want an error")
	}
}
