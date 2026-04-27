package store

import (
	"context"
	"testing"
)

func TestWithPrecomputedEmbeddings_RoundTrip(t *testing.T) {
	vecA := []float32{0.1, 0.2, 0.3}
	vecB := []float32{0.4, 0.5}
	ctx := WithPrecomputedEmbeddings(context.Background(), map[string][]float32{
		"kb/a.md": vecA,
		"kb/b.md": vecB,
	})

	got, ok := precomputedEmbedding(ctx, "kb/a.md")
	if !ok {
		t.Fatal("expected hit for kb/a.md, got miss")
	}
	if len(got) != 3 || got[0] != 0.1 {
		t.Errorf("kb/a.md vector mismatch: got %v", got)
	}
	got, ok = precomputedEmbedding(ctx, "kb/b.md")
	if !ok {
		t.Fatal("expected hit for kb/b.md, got miss")
	}
	if len(got) != 2 {
		t.Errorf("kb/b.md vector length: got %d want 2", len(got))
	}
}

func TestWithPrecomputedEmbeddings_MissFallsThrough(t *testing.T) {
	ctx := WithPrecomputedEmbeddings(context.Background(), map[string][]float32{
		"kb/a.md": {0.1},
	})
	if _, ok := precomputedEmbedding(ctx, "kb/missing.md"); ok {
		t.Error("expected miss for absent path, got hit")
	}
}

// TestPrecomputedEmbedding_NoCacheAttached verifies that calling
// precomputedEmbedding on a context that never went through
// WithPrecomputedEmbeddings returns (nil, false) — the indexer's signal
// to fall back to the embedder. Crucial for backward compatibility with
// every caller that doesn't donate.
func TestPrecomputedEmbedding_NoCacheAttached(t *testing.T) {
	if vec, ok := precomputedEmbedding(context.Background(), "kb/a.md"); ok {
		t.Errorf("expected miss on bare ctx, got hit with %v", vec)
	}
}

// TestWithPrecomputedEmbeddings_EmptyMapNoOp confirms passing an empty or
// nil map does not attach a cache at all — avoids polluting downstream
// contexts with no-op lookups when the caller had no vectors to donate.
func TestWithPrecomputedEmbeddings_EmptyMapNoOp(t *testing.T) {
	ctx := WithPrecomputedEmbeddings(context.Background(), nil)
	if _, ok := precomputedEmbedding(ctx, "kb/a.md"); ok {
		t.Error("expected miss after WithPrecomputedEmbeddings(nil)")
	}
	ctx = WithPrecomputedEmbeddings(context.Background(), map[string][]float32{})
	if _, ok := precomputedEmbedding(ctx, "kb/a.md"); ok {
		t.Error("expected miss after WithPrecomputedEmbeddings(empty)")
	}
}

// TestPrecomputedEmbedding_WrongValueTypeIgnored guards the type
// assertion: if some other package somehow stashed a value under a
// colliding key (impossible today since the key type is unexported, but
// belt-and-suspenders), the lookup must not panic and must report miss.
func TestPrecomputedEmbedding_WrongValueTypeIgnored(t *testing.T) {
	ctx := context.WithValue(context.Background(), embeddingCacheKey{}, "not a map")
	if _, ok := precomputedEmbedding(ctx, "kb/a.md"); ok {
		t.Error("expected miss when context value is wrong type")
	}
}
