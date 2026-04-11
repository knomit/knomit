// Package testenv is the Storyboard DSL for knomit invariant tests. It is a
// thin wrapper over real store and repos APIs — every DSL method maps to one
// or two real production calls. The DSL has no logic of its own beyond
// plumbing and capturing commit hashes.
package testenv

import (
	"crypto/sha256"
	"encoding/binary"
)

// embeddingDim matches the facts_vec schema's hard-coded vec0 dimension.
// vec0 rejects inserts whose vector length does not match, so this is not
// configurable.
const embeddingDim = 768

// DeterministicEmbedder implements store.BatchEmbedder by hashing input text
// into a fixed 768-dim float32 vector. Same input always yields the same
// vector. Test-only.
//
// Matches the store.Embedder interface: Embed(text string) ([]float32, error)
// plus the BatchEmbedder extension: EmbedBatch(texts []string) ([][]float32, error).
// There is no Dimension() method — the dimension is schema-fixed by vec0.
type DeterministicEmbedder struct{}

// Embed implements store.Embedder.
func (e *DeterministicEmbedder) Embed(text string) ([]float32, error) {
	return e.vectorFor(text), nil
}

// EmbedBatch implements store.BatchEmbedder.
func (e *DeterministicEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vectorFor(t)
	}
	return out, nil
}

func (e *DeterministicEmbedder) vectorFor(text string) []float32 {
	out := make([]float32, embeddingDim)
	// Stretch sha256 across all 768 floats. sha256 produces 32 bytes = 8
	// float32s; we re-hash with a counter to extend.
	for i := range embeddingDim {
		h := sha256.Sum256(append([]byte{byte(i), byte(i >> 8)}, []byte(text)...))
		bits := binary.LittleEndian.Uint32(h[:4])
		// Map uint32 to a float32 in [-1, 1].
		out[i] = float32(int32(bits)) / float32(1<<31)
	}
	return out
}
