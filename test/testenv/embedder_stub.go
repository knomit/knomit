// Package testenv is the Storyboard DSL for knomit invariant tests. It is a
// thin wrapper over real store and repos APIs — every DSL method maps to one
// or two real production calls. The DSL has no logic of its own beyond
// plumbing and capturing commit hashes.
package testenv

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"knomit/internal/retrieval"
)

// embeddingDim is the default vector dimension used when DimOverride is unset.
// facts_vec is created with the active embedder's dim, so the dimension is
// dynamic per model; this constant is only the stub's default.
const embeddingDim = 768

// DeterministicEmbedder implements store.BatchEmbedder by hashing input text
// into a deterministic float32 vector (768-dim by default). Same input always
// yields the same vector. Test-only.
//
// Matches the role-aware store.Embedder interface (EmbedQuery, EmbedDocument,
// Dim, ID) plus the BatchEmbedder extension (EmbedDocuments). Document role
// hashes title+body so callers get a stable vector for the same fact content.
//
// The zero value behaves as a fixed model with ID "deterministic-stub" and
// dim 768. Tests that need to model a config change to a different embedding
// model set IDOverride (and optionally DimOverride) to vary identity.
type DeterministicEmbedder struct {
	// IDOverride, when non-empty, is returned by ID(); else "deterministic-stub".
	IDOverride string
	// DimOverride, when >0, is returned by Dim() and sizes vectors; else 768.
	DimOverride int
}

// EmbedQuery implements store.Embedder.
func (e *DeterministicEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return e.vectorFor(text), nil
}

// EmbedDocument implements store.Embedder.
func (e *DeterministicEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.vectorFor(title + " " + body), nil
}

// dim returns the configured vector dimension (DimOverride if set, else 768).
func (e *DeterministicEmbedder) dim() int {
	if e.DimOverride > 0 {
		return e.DimOverride
	}
	return embeddingDim
}

// Dim implements store.Embedder.
func (e *DeterministicEmbedder) Dim() int { return e.dim() }

// Thresholds implements store.Embedder. Tests use the historical defaults so
// dedup/search/graph behaviour is independent of which model id the stub mimics.
func (e *DeterministicEmbedder) Thresholds() retrieval.Thresholds { return retrieval.Defaults() }

// ID implements store.Embedder.
func (e *DeterministicEmbedder) ID() string {
	if e.IDOverride != "" {
		return e.IDOverride
	}
	return "deterministic-stub"
}

// EmbedDocuments implements store.BatchEmbedder.
func (e *DeterministicEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vectorFor(titles[i] + " " + bodies[i])
	}
	return out, nil
}

func (e *DeterministicEmbedder) vectorFor(text string) []float32 {
	d := e.dim()
	out := make([]float32, d)
	// Stretch sha256 across all floats. sha256 produces 32 bytes = 8
	// float32s; we re-hash with a counter to extend.
	for i := range d {
		h := sha256.Sum256(append([]byte{byte(i), byte(i >> 8)}, []byte(text)...))
		bits := binary.LittleEndian.Uint32(h[:4])
		// Map uint32 to a float32 in [-1, 1].
		out[i] = float32(int32(bits)) / float32(1<<31)
	}
	return out
}
