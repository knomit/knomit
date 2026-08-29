package embeddings

import (
	"context"
	"fmt"
	"sort"
)

// DefaultMaxBatchTokens is the padded-token budget for one ONNX session.Run
// when the operator has configured none.
//
// Memory scales with the PADDED token count — rows × the longest row in the
// batch, since embedBatch pads every row to the longest — not with the document
// count. Measured on embeddinggemma (see
// .claude/plans/2026-08-29-embed-memory-results.txt), holding batch × seq at
// this value: 128×128 = 1154 MiB, 64×256 = 1159 MiB, 256×64 = 1144 MiB. Within
// 1.3% of each other, which is why the budget bounds the product rather than
// either factor.
//
// The worst shape this budget permits is 8 rows × 2048 tokens (MaxTokens),
// measured at 1820 MiB above the ~675 MiB resident model. The fixed count it
// replaces — 32 documents — reached 9030 MiB on the same corpus, which is what
// OOM-killed the server: search_index.go's rebuild chunk was also 32, so the
// two constants being equal made every long chunk exactly one maximal run.
const DefaultMaxBatchTokens = 16384

// minRowCharge is the floor each row is charged when packing, whatever its real
// token length. It caps a batch at DefaultMaxBatchTokens/minRowCharge = 256
// rows, which the token budget alone would not: at 2048 rows of 8 tokens the
// measured delta is 1683 MiB against ~1150 MiB for the same token count in
// fewer, longer rows — a per-row cost of ~0.26 MiB that the token model does
// not capture.
//
// It also preserves the cancellation contract documented on store.Embedder
// ("cancelling bounds latency to one batch"). Two callers are bounded by no
// constant at all — learn.go embeds an entire incoming write request, and
// motif_alias.go embeds a whole motif vocabulary in one call — so without this
// floor either could become a single uninterruptible session.Run.
const minRowCharge = 64

// encodedRow is one tokenized text: input ids and attention mask, already
// truncated to the model's MaxTokens. Rows are carried rather than strings so
// packing can see token lengths without tokenizing twice.
type encodedRow struct {
	ids  []int64
	mask []int64
}

// charge is the width a row is billed at when packing. Never below
// minRowCharge, and never below 1 — packByTokenBudget divides by it, and a
// zero-token encoding is a real input (embedBatch guards maxLen == 0 for the
// same reason), so a zero charge would panic on an empty document.
func charge(tokenLen int) int {
	if tokenLen < minRowCharge {
		return minRowCharge
	}
	return tokenLen
}

// packByTokenBudget groups row indices into batches whose padded cost — rows ×
// the widest row — stays within budget. Returns indices into lens, so callers
// must scatter results back to those positions.
//
// Rows are ordered by charged width, descending. That does two jobs: it makes
// the first row's width the batch maximum, so k×L ≤ budget holds by
// construction, and it groups similar lengths together so padding waste stays
// low.
//
// Two rows are not negotiable. A row wider than the whole budget still runs, as
// a batch of one — a document cannot be split, so the budget is a target with
// exactly one documented exception. And a non-positive budget degrades to one
// row per batch rather than looping forever; that is unreachable through config
// (app resolves 0 to the default, Validate rejects negatives) and guards
// programmer error only.
func packByTokenBudget(lens []int, budget int) [][]int {
	if len(lens) == 0 {
		return nil
	}

	idx := make([]int, len(lens))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return charge(lens[idx[a]]) > charge(lens[idx[b]])
	})

	var batches [][]int
	for i := 0; i < len(idx); {
		width := charge(lens[idx[i]])
		k := budget / width
		if k < 1 {
			k = 1
		}
		end := i + k
		if end > len(idx) {
			end = len(idx)
		}

		// Stop at a length cliff. Descending order keeps neighbours similar, but
		// where the corpus steps down sharply a wide row would otherwise drag
		// narrow ones up to its width — and padding is real ONNX compute, not
		// just memory. Compare doubled rather than halved to avoid the rounding
		// that integer division would introduce at small widths.
		for j := i + 1; j < end; j++ {
			if charge(lens[idx[j]])*2 < width {
				end = j
				break
			}
		}

		batches = append(batches, idx[i:end])
		i = end
	}
	return batches
}

// embedInBatches packs rows into budget-bounded batches, runs each, and
// restores every result to its INPUT position. The reordering is why that last
// step matters: callers correlate the returned slice positionally with the
// titles/bodies they passed (search_index.go's rebuild, learn.go's dedup,
// every EmbedShortStrings consumer), so scattering wrongly would give each
// fact its neighbour's vector — a corruption no length check would catch.
//
// ctx is checked at entry and before each batch. It is a checkpoint, not an
// abort: sess.Run cannot be interrupted, so cancelling bounds latency to one
// batch, exactly as store.Embedder documents.
func embedInBatches(ctx context.Context, rows []encodedRow, budget int, run func([]encodedRow) ([][]float32, error)) ([][]float32, error) {
	// Checked at entry as well as per batch: with no rows the loop body never
	// runs, and returning (empty, nil) on a cancelled context would break the
	// "observed at entry to each call" promise the Embedder interface makes.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lens := make([]int, len(rows))
	for i, r := range rows {
		lens[i] = len(r.ids)
	}

	out := make([][]float32, len(rows))
	for _, batch := range packByTokenBudget(lens, budget) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		sub := make([]encodedRow, len(batch))
		for j, at := range batch {
			sub[j] = rows[at]
		}

		vecs, err := run(sub)
		if err != nil {
			// Return nothing rather than the partially scattered slice: it would
			// be the right length with nil holes, which a caller checking only
			// len() would read as success.
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("embedInBatches: run returned %d vectors for %d rows", len(vecs), len(batch))
		}

		for j, at := range batch {
			out[at] = vecs[j]
		}
	}
	return out, nil
}
