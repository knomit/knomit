package store

import "context"

// embeddingCacheKey is unexported so only this package can stash and read
// the value. Callers from other packages go through WithPrecomputedEmbeddings.
type embeddingCacheKey struct{}

// WithPrecomputedEmbeddings returns a context carrying a path → vector
// lookup. The indexer consults it inside upsert before invoking the
// configured embedder, allowing callers that already produced a vector
// for the content they are about to write (e.g. mcp/learn after its
// dedup pass) to skip a redundant ONNX inference. Missing entries fall
// through to normal embedding.
//
// Donated vectors are trusted blindly — there is no validation that the
// vector was computed over the same text the indexer is about to
// persist. Callers MUST drop entries when they choose to write content
// other than what they embedded (e.g. dedup-merge "existing wins"
// branches, where the merged fact carries the existing fact's title and
// body, not the new submission's). A wrong vector here means the fact's
// vec0 row will not match its on-disk content and similarity search
// quality will degrade silently.
func WithPrecomputedEmbeddings(ctx context.Context, byPath map[string][]float32) context.Context {
	if len(byPath) == 0 {
		return ctx
	}
	return context.WithValue(ctx, embeddingCacheKey{}, byPath)
}

// precomputedEmbedding returns a donated vector for path if the context
// carries one. ok=false means "no cache attached or no entry for this
// path" — caller should fall back to embedding.
func precomputedEmbedding(ctx context.Context, path string) ([]float32, bool) {
	m, ok := ctx.Value(embeddingCacheKey{}).(map[string][]float32)
	if !ok {
		return nil, false
	}
	vec, ok := m[path]
	return vec, ok
}
