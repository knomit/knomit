// Package retrieval holds value types shared by the embedding, store, and
// synthesis layers without coupling them to each other. It is a dependency-free
// leaf: importing it never pulls in cgo (embeddings) or the store.
package retrieval

// Thresholds are the model-dependent cosine cutoffs used across retrieval and
// dedup. Every value is an absolute point on ONE embedding model's cosine
// distribution, so a different model needs different values — switching models
// without re-deriving these silently mis-tunes dedup, graph density, and search
// recall. Each embeddings.Model carries its own set; see tools/calibrate for
// how they are derived (distribution-preserving port from the previous model).
type Thresholds struct {
	// Dedup is the cosine floor at which a new fact is treated as a
	// near-duplicate of an existing one and merged (learn-time + synthesis
	// prune). Over-merge is data-lossy, so this is the load-bearing one.
	Dedup float64
	// ReflectNovelty is the floor above which a proposed methodology is
	// rejected as too similar to an existing one.
	ReflectNovelty float64
	// SimilarTo is the floor for drawing a SIMILAR_TO graph edge between two
	// facts (applied on top of the top-K nearest-neighbour cap).
	SimilarTo float64
	// SearchFloor is the default recall floor for vector search, used when a
	// caller does not supply its own MinSimilarity.
	SearchFloor float64
	// RerankHigh / RerankLow bucket a caller-supplied MinSimilarity to decide
	// how many candidates to over-fetch before reranking. RerankHigh > RerankLow.
	RerankHigh float64
	RerankLow  float64
}

// Defaults returns the historical nomic-era values. They are the fallback when
// no embedder is configured (embeddings disabled), keeping behaviour identical
// to before thresholds became model-dependent.
func Defaults() Thresholds {
	return Thresholds{
		Dedup:          0.92,
		ReflectNovelty: 0.85,
		SimilarTo:      0.60,
		SearchFloor:    0.40,
		RerankHigh:     0.70,
		RerankLow:      0.50,
	}
}
