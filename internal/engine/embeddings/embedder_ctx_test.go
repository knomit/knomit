package embeddings

import (
	"context"
	"errors"
	"testing"
)

// The batching-loop cancellation tests that used to live here moved to
// batching_test.go when embedInBatches changed from fixed docBatchSize chunks
// to token-budget packing: TestEmbedInBatches_CancelledBetweenBatches carries
// all three of their cases forward. What remains is the single-shot companion,
// which does not go through the batching loop at all.

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
