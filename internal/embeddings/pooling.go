package embeddings

import "math"

// poolMean averages last_hidden_state over attended tokens.
// data is [seqLen*dim] for a single sequence; mask is [seqLen].
func poolMean(data []float32, mask []int64, seqLen, dim int) []float32 {
	out := make([]float32, dim)
	var n float64
	for t := 0; t < seqLen; t++ {
		if mask[t] == 0 {
			continue
		}
		n++
		off := t * dim
		for d := 0; d < dim; d++ {
			out[d] += data[off+d]
		}
	}
	if n > 0 {
		inv := float32(1.0 / n)
		for d := range out {
			out[d] *= inv
		}
	}
	return out
}

// poolCLS returns token 0.
func poolCLS(data []float32, dim int) []float32 {
	out := make([]float32, dim)
	copy(out, data[:dim])
	return out
}

// poolLastToken returns the last attended (non-pad) token.
func poolLastToken(data []float32, mask []int64, seqLen, dim int) []float32 {
	last := 0
	for t := 0; t < seqLen; t++ {
		if mask[t] != 0 {
			last = t
		}
	}
	out := make([]float32, dim)
	copy(out, data[last*dim:last*dim+dim])
	return out
}

// l2normalize scales v to unit length in place.
func l2normalize(v []float32) {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	n = math.Sqrt(n)
	if n > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / n)
		}
	}
}
