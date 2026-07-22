package embeddings

import (
	"context"
	"errors"
	"testing"
)

// TestEmbedDocuments_CancelledBetweenBatches pins the per-batch cancellation
// checkpoint in the EmbedDocuments loop. Cancelling a full-corpus re-embed must
// abandon the remaining batches rather than grinding through the whole corpus;
// the checkpoint is what bounds cancellation latency to one batch.
//
// Driven through embedInBatches with a counting run func so the assertion is
// about the loop, not about ONNX — the real sess.Run is uninterruptible and
// needs a cached model, neither of which this invariant depends on.
func TestEmbedDocuments_CancelledBetweenBatches(t *testing.T) {
	texts := make([]string, docBatchSize*3) // three batches' worth
	for i := range texts {
		texts[i] = "doc"
	}

	t.Run("cancelled after the first batch", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		batches := 0
		run := func(chunk []string) ([][]float32, error) {
			batches++
			cancel() // caller gives up while this batch is in flight
			return make([][]float32, len(chunk)), nil
		}
		_, err := embedInBatches(ctx, texts, run)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if batches != 1 {
			t.Errorf("ran %d batches after cancellation, want 1 (the in-flight one)", batches)
		}
	})

	t.Run("pre-cancelled runs no inference at all", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		batches := 0
		run := func(chunk []string) ([][]float32, error) {
			batches++
			return make([][]float32, len(chunk)), nil
		}
		_, err := embedInBatches(ctx, texts, run)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if batches != 0 {
			t.Errorf("ran %d batches on a pre-cancelled ctx, want 0", batches)
		}
	})
}

// TestEmbedQuery_CancelledContext is the single-shot companion: a pre-cancelled
// context must short-circuit before inference. The embedder here is a zero
// value with no ONNX session or tokenizer, so reaching inference would panic —
// that is deliberately the assertion mechanism, since a nil session is the only
// "inference did not run" counter available without a cached model.
func TestEmbedQuery_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e := &Embedder{model: Model{QueryTemplate: "search_query: {content}"}}
	if _, err := e.EmbedQuery(ctx, "anything"); !errors.Is(err, context.Canceled) {
		t.Fatalf("EmbedQuery err = %v, want context.Canceled", err)
	}

	e = &Embedder{model: Model{DocTemplate: "search_document: {content}"}}
	if _, err := e.EmbedDocument(ctx, "t", "b"); !errors.Is(err, context.Canceled) {
		t.Fatalf("EmbedDocument err = %v, want context.Canceled", err)
	}
}

// TestEmbedInBatches_EmptyInput_StillObservesCancellation closes the one gap in
// the "ctx is observed at entry to each call" contract the Embedder interface
// documents: with no texts the loop body never runs, so a fully cancelled
// context used to yield (empty, nil) — a success. EmbedQuery and EmbedDocument
// both check at entry, and callers that branch on the error rather than the
// (empty) result would silently treat a cancelled re-embed as a completed one.
func TestEmbedInBatches_EmptyInput_StillObservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	run := func(chunk []string) ([][]float32, error) {
		t.Fatal("run must not be called for empty input")
		return nil, nil
	}
	if _, err := embedInBatches(ctx, nil, run); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
