package embeddings

import (
	"math"
	"testing"
)

func TestPoolMean(t *testing.T) {
	data := []float32{1, 2, 3, 4} // 2 tokens, dim 2: [[1,2],[3,4]] -> mean [2,3]
	got := poolMean(data, []int64{1, 1}, 2, 2)
	if got[0] != 2 || got[1] != 3 {
		t.Errorf("poolMean = %v, want [2 3]", got)
	}
}

func TestPoolMeanIgnoresPadding(t *testing.T) {
	data := []float32{1, 2, 3, 4}
	got := poolMean(data, []int64{1, 0}, 2, 2) // 2nd token padding -> mean == first
	if got[0] != 1 || got[1] != 2 {
		t.Errorf("poolMean(pad) = %v, want [1 2]", got)
	}
}

func TestL2Normalize(t *testing.T) {
	v := []float32{3, 4}
	l2normalize(v)
	if math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Errorf("l2normalize = %v, want [0.6 0.8]", v)
	}
}
