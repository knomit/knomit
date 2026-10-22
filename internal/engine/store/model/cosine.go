package model

import (
	"math"
)

// CosineSim is the cosine similarity of two vectors, and the single definition
// of that formula in this package: the SQL function knomit_cosine_sim decodes
// its blobs and calls straight through to CosineSimOK, so a change here cannot
// leave the two disagreeing.
//
// Returns 0 when there is no defined similarity — mismatched lengths, or a
// degenerate (zero-norm) vector. Callers that must distinguish "no similarity"
// from a genuine 0 (orthogonal vectors have a perfectly good similarity, and it
// is 0) use CosineSimOK instead.
func CosineSim(a, b []float32) float64 {
	sim, _ := CosineSimOK(a, b)
	return sim
}

// CosineSimOK reports the similarity and whether it is DEFINED. The bool is the
// distinction the SQL function needs and the shortlist does not: SQL answers
// NULL for a degenerate vector, while a threshold comparison treats "undefined"
// and "far apart" the same way.
func CosineSimOK(a, b []float32) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, normA, normB float64
	for i := range a {
		va, vb := float64(a[i]), float64(b[i])
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0, false
	}
	return dot / denom, true
}
