package cluster

import (
	"math/rand"
	"testing"
)

func TestHDBSCAN(t *testing.T) {
	// 3 tight groups of 4 points in 2D, well-separated
	points := [][]float64{
		{0, 0}, {0.1, 0}, {0, 0.1}, {0.1, 0.1}, // cluster 0
		{5, 5}, {5.1, 5}, {5, 5.1}, {5.1, 5.1}, // cluster 1
		{10, 0}, {10.1, 0}, {10, 0.1}, {10.1, 0.1}, // cluster 2
	}
	labels := HDBSCAN(points, HDBSCANOptions{MinClusterSize: 3})
	if len(labels) != len(points) {
		t.Fatalf("expected %d labels, got %d", len(points), len(labels))
	}
	// Expect 3 non-noise labels
	unique := map[int]bool{}
	for _, l := range labels {
		if l != -1 {
			unique[l] = true
		}
	}
	if len(unique) != 3 {
		t.Fatalf("expected 3 clusters, got %v (labels: %v)", len(unique), labels)
	}
}

func TestHDBSCANAllNoise(t *testing.T) {
	// Spread-out points that cannot form clusters of size 5
	points := [][]float64{
		{0, 0}, {100, 100}, {200, 0}, {0, 200}, {100, 200},
	}
	labels := HDBSCAN(points, HDBSCANOptions{MinClusterSize: 5})
	if len(labels) != 5 {
		t.Fatalf("expected 5 labels, got %d", len(labels))
	}
	// With only 5 points and minClusterSize=5, at most one cluster but points
	// are well separated so expect all noise or one degenerate cluster.
	// The test just verifies no panic and correct label count.
}

func TestHDBSCANTwoClusters(t *testing.T) {
	points := [][]float64{
		{0, 0}, {0.05, 0}, {0, 0.05}, {0.05, 0.05}, {0.02, 0.02},
		{10, 10}, {10.05, 10}, {10, 10.05}, {10.05, 10.05}, {10.02, 10.02},
	}
	labels := HDBSCAN(points, HDBSCANOptions{MinClusterSize: 4})
	unique := map[int]bool{}
	for _, l := range labels {
		if l != -1 {
			unique[l] = true
		}
	}
	if len(unique) != 2 {
		t.Fatalf("expected 2 clusters, got %d (labels: %v)", len(unique), labels)
	}
}

func TestSplitByMetadata(t *testing.T) {
	facts := []FactMeta{
		{Domain: []string{"go", "testing"}, Entities: []string{}},
		{Domain: []string{"go"}, Entities: []string{"pkg/foo"}},
		{Domain: []string{"testing"}, Entities: []string{}},
		// Disconnected group: different domain, no overlap
		{Domain: []string{"database"}, Entities: []string{"postgres"}},
		{Domain: []string{"database"}, Entities: []string{}},
		{Domain: []string{"sql"}, Entities: []string{"postgres"}},
	}
	// All in cluster 0 per HDBSCAN
	labels := []int{0, 0, 0, 0, 0, 0}

	result := SplitByMetadata(facts, labels, 2)

	// Expect two components: {0,1,2} (linked via "go" and "testing") and {3,4,5} (linked via "database" and "postgres")
	clusters := map[int][]int{}
	noise := []int{}
	for i, l := range result {
		if l == -1 {
			noise = append(noise, i)
		} else {
			clusters[l] = append(clusters[l], i)
		}
	}

	if len(clusters) != 2 {
		t.Fatalf("expected 2 sub-clusters after split, got %d (result: %v)", len(clusters), result)
	}
	if len(noise) != 0 {
		t.Fatalf("expected no noise, got %v", noise)
	}
}

func TestSplitByMetadataSingletonNoise(t *testing.T) {
	facts := []FactMeta{
		{Domain: []string{"go"}, Entities: []string{}},
		{Domain: []string{"go"}, Entities: []string{}},
		{Domain: []string{"go"}, Entities: []string{}},
		{Domain: []string{"isolated"}, Entities: []string{}}, // alone
	}
	labels := []int{0, 0, 0, 0}
	result := SplitByMetadata(facts, labels, 3)

	noise := 0
	clusterCount := map[int]bool{}
	for _, l := range result {
		if l == -1 {
			noise++
		} else {
			clusterCount[l] = true
		}
	}
	if len(clusterCount) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusterCount))
	}
	if noise != 1 {
		t.Fatalf("expected 1 noise point, got %d", noise)
	}
}

func TestSplitByMetadataNoTags(t *testing.T) {
	facts := []FactMeta{
		{Domain: []string{}, Entities: []string{}},
		{Domain: []string{}, Entities: []string{}},
		{Domain: []string{}, Entities: []string{}},
	}
	labels := []int{0, 0, 0}
	result := SplitByMetadata(facts, labels, 2)
	// All in one component when no tags
	clusterCount := map[int]bool{}
	for _, l := range result {
		if l != -1 {
			clusterCount[l] = true
		}
	}
	if len(clusterCount) != 1 {
		t.Fatalf("expected 1 cluster, got %d (result: %v)", len(clusterCount), result)
	}
}

func TestClusterFacts(t *testing.T) {
	// Smoke test: 12 embeddings with clear cluster structure
	// 3 groups of 4 points in 8-dim space
	rng := rand.New(rand.NewSource(42))
	makeCluster := func(center []float32) []float32 {
		v := make([]float32, len(center))
		for i, c := range center {
			v[i] = c + float32(rng.NormFloat64())*0.05
		}
		return v
	}

	c0 := []float32{1, 0, 0, 0, 0, 0, 0, 0}
	c1 := []float32{0, 0, 0, 1, 0, 0, 0, 0}
	c2 := []float32{0, 0, 0, 0, 0, 0, 0, 1}

	embeddings := make([][]float32, 12)
	metas := make([]FactMeta, 12)
	for i := 0; i < 4; i++ {
		embeddings[i] = makeCluster(c0)
		metas[i] = FactMeta{Domain: []string{"a"}, Entities: []string{}}
	}
	for i := 4; i < 8; i++ {
		embeddings[i] = makeCluster(c1)
		metas[i] = FactMeta{Domain: []string{"b"}, Entities: []string{}}
	}
	for i := 8; i < 12; i++ {
		embeddings[i] = makeCluster(c2)
		metas[i] = FactMeta{Domain: []string{"c"}, Entities: []string{}}
	}

	result, err := ClusterFacts(embeddings, metas, ClusterOptions{
		UMAPDimensions: 2,
		MinClusterSize: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify we got some clusters (exact count may vary due to UMAP stochasticity)
	if len(result.Clusters) == 0 && len(result.Noise) == 12 {
		t.Fatal("expected some clusters, got all noise")
	}

	// Verify all indices are accounted for
	seen := make(map[int]bool)
	for _, idxs := range result.Clusters {
		for _, idx := range idxs {
			if seen[idx] {
				t.Fatalf("index %d appears twice", idx)
			}
			seen[idx] = true
		}
	}
	for _, idx := range result.Noise {
		if seen[idx] {
			t.Fatalf("noise index %d already in a cluster", idx)
		}
		seen[idx] = true
	}
	if len(seen) != 12 {
		t.Fatalf("expected 12 total indices, got %d", len(seen))
	}
}
