package cluster

import (
	"math"
	"math/rand"
	"testing"
)

func TestUMAP(t *testing.T) {
	// 20 random 384-dim vectors
	vecs := make([][]float64, 20)
	rng := rand.New(rand.NewSource(99))
	for i := range vecs {
		vecs[i] = make([]float64, 384)
		for j := range vecs[i] {
			vecs[i][j] = rng.Float64()
		}
	}
	result, err := UMAP(vecs, UMAPOptions{
		NComponents: 5,
		NNeighbors:  10,
		MinDist:     0.1,
		Seed:        42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 20 || len(result[0]) != 5 {
		t.Fatalf("expected 20x5, got %dx%d", len(result), len(result[0]))
	}
	for i, row := range result {
		for j, v := range row {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("result[%d][%d] = %v (NaN or Inf)", i, j, v)
			}
		}
	}
}

func TestUMAPSingleVector(t *testing.T) {
	_, err := UMAP([][]float64{{1.0, 2.0}}, UMAPOptions{
		NComponents: 2,
		NNeighbors:  1,
		MinDist:     0.1,
		Seed:        1,
	})
	if err == nil {
		t.Fatal("expected error for n=1, got nil")
	}
}

func TestUMAPMinNeighbors(t *testing.T) {
	// Ensure NNeighbors is clamped when n is small
	vecs := make([][]float64, 5)
	rng := rand.New(rand.NewSource(7))
	for i := range vecs {
		vecs[i] = make([]float64, 32)
		for j := range vecs[i] {
			vecs[i][j] = rng.Float64()
		}
	}
	result, err := UMAP(vecs, UMAPOptions{
		NComponents: 2,
		NNeighbors:  15, // will be clamped to n-1=4
		MinDist:     0.1,
		Seed:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 5 || len(result[0]) != 2 {
		t.Fatalf("expected 5x2, got %dx%d", len(result), len(result[0]))
	}
}
