package synthesize

import "testing"

func TestClusterResultClusterOf(t *testing.T) {
	cr := ClusterResult{Clusters: map[int][]string{0: {"a", "b"}, 1: {"c"}}, Noise: []string{"d", "e"}}
	m := cr.ClusterOf()
	if m["a"] != 0 || m["b"] != 0 || m["c"] != 1 {
		t.Fatalf("clustered ids wrong: %+v", m)
	}
	if m["d"] == m["e"] {
		t.Errorf("noise paths must get distinct ids: %+v", m)
	}
	if m["d"] >= 0 || m["e"] >= 0 {
		t.Errorf("noise ids must be negative: %+v", m)
	}
}

func TestClusterResultFromGroups(t *testing.T) {
	groups := [][]factForLLM{
		{{File: "a"}, {File: "b"}},
		{{File: "c"}},
	}
	cr := clusterResultFromGroups(groups)
	if len(cr.Clusters) != 2 {
		t.Fatalf("want 2 communities, got %d: %+v", len(cr.Clusters), cr.Clusters)
	}
	if len(cr.Noise) != 0 {
		t.Errorf("groups carry no noise; Noise must be empty: %+v", cr.Noise)
	}
	m := cr.ClusterOf()
	if m["a"] != m["b"] {
		t.Errorf("a and b share a group, must map to same community: %+v", m)
	}
	if m["a"] == m["c"] {
		t.Errorf("a and c are distinct groups, must differ: %+v", m)
	}
}
