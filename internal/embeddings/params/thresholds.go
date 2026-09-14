// Package params holds the cgo-free half of the embedding-model contract:
// everything a non-cgo caller needs to know about a model, without linking one.
//
// It sits under internal/embeddings because that is whose values these are — a
// model's calibrated cutoffs and the shipped default model id — but it does NOT
// import its parent, and must never start. The parent carries cgo (ONNX via
// import "C"), so an import edge from here upward would silently link the ONNX
// runtime into internal/store, internal/config and every one of their
// consumers. Being importable by the store, the config loader and the synthesis
// pipelines without dragging in cgo is the entire reason this package exists.
//
// That invariant is enforced, not merely documented: TestParamsHasNoDependencies
// in test/archtest fails if this package ever acquires a non-stdlib import.
package params

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

// nomicModelID is the historical default, kept here because Defaults() is its
// alias: the fallback IS a model's calibration, not a separate set of numbers.
const nomicModelID = "nomic-v1.5"

// modelThresholds is every shipped model's calibrated set, as pure data.
//
// It lives HERE rather than beside each descriptor in the cgo parent so the
// cgo-free half of the contract knows a model's GEOMETRY as well as its
// identity. Before this, params held DefaultModelID but not the thresholds it
// names, so any non-cgo caller wanting the shipped band had to either import
// the ONNX-linked parent or retype the numbers — and a retyped copy goes stale
// silently at the next re-sweep. The descriptors in internal/embeddings read
// from this map, so there is exactly one place a calibration lands.
//
// This is data, not an import: TestParamsHasNoDependencies bans non-stdlib
// imports, and a map of float64 adds none.
var modelThresholds = map[string]Thresholds{
	// The historical nomic-era values, and the fallback when no embedder is
	// configured (embeddings disabled) — behaviour identical to before
	// thresholds became model-dependent.
	nomicModelID: {
		Dedup:          0.92,
		ReflectNovelty: 0.85,
		SimilarTo:      0.60,
		SearchFloor:    0.40,
		RerankHigh:     0.70,
		RerankLow:      0.50,
	},
	// Calibrated against the real knomit corpus (712 facts, tools/calibrate).
	// EmbeddingGemma's cosine distribution runs markedly cooler than nomic's
	// (distinct same-category pairs: mean 0.48 vs 0.75), so every cutoff is
	// ported DOWN by preserving the percentile it occupied on nomic. Dedup
	// 0.82 sits in the validated safety gap (distinct p99 0.77 < 0.82 < true
	// near-dup p05 0.96). SearchFloor's pure port was ~0, clamped to 0.05 to
	// drop only anti-correlated noise.
	DefaultModelID: {
		Dedup:          0.82,
		ReflectNovelty: 0.69,
		SimilarTo:      0.18,
		SearchFloor:    0.05,
		RerankHigh:     0.43,
		RerankLow:      0.10,
	},
}

// ForModel returns the calibrated thresholds for a model id, and whether that
// id has an entry at all.
//
// THE BOOL IS LOAD-BEARING: never collapse this to "return the entry, or
// Defaults()". A silent fallback would judge an unknown or typo'd model id
// against NOMIC geometry with nothing red — the same defect as returning a
// default for an unknown motif id (bridge_motif's "UNKNOWN, not defaulted",
// review finding M-5), and the reason
// kb/invariants/integrations/hooks/guards-fail-closed requires a guard to
// distinguish absent from zero rather than proceed on a zero value.
//
// Presence is a MAP-KEY PROBE, deliberately, and must stay one. Implementing it
// as `th == Defaults()` would report nomic-v1.5 — a real, registered model
// whose calibration IS Defaults() — as missing. This mirrors requireResponseKey
// probing for a key's presence rather than for a non-empty value, for the same
// reason: a legitimate value that happens to equal the zero/default case is
// still a value.
//
// Absence means "we have no calibration for this model", so a caller that
// cannot evaluate must take the NON-aggressive branch. It does NOT license
// validating ids: false is for a genuinely absent entry, never for an id that
// merely looks unfamiliar.
func ForModel(id string) (Thresholds, bool) {
	th, ok := modelThresholds[id]
	return th, ok
}

// Defaults returns the historical nomic-era values. They are the fallback when
// no embedder is configured (embeddings disabled), keeping behaviour identical
// to before thresholds became model-dependent.
func Defaults() Thresholds { return modelThresholds[nomicModelID] }
