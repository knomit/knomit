package synthesize

import (
	"fmt"
	"sort"
	"testing"

	"go.uber.org/mock/gomock"
	"knomit/internal/store"
)

func TestScopedCluster_EmptySeeds(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	result, err := ScopedCluster(nil, idx, 1.0, nil, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %v", result)
	}
}

func TestScopedCluster_SeedsAndNeighborsClustered(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	seed := factForLLM{File: "kb/go/concurrency/channels.md", Title: "Go channels", Body: "channels in go"}

	// Search returns the neighbor for the seed's category.
	idx.EXPECT().Search(gomock.Any(), store.SearchQuery{
		Text:  "Go channels channels in go",
		Path:  "kb/go/concurrency",
		Limit: 10,
	}).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/concurrency/goroutines.md", Title: "Goroutines"}, Body: "goroutines"}, Score: 80},
	}, nil)

	// ClusterFacts returns clusters that include both seed and neighbor in one cluster,
	// and the unrelated fact in another.
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/go/concurrency/channels.md", "kb/go/concurrency/goroutines.md"},
			1: {"kb/python/async/await.md"},
		},
	}, nil)

	result, err := ScopedCluster([]factForLLM{seed}, idx, 1.0, nil, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should get 1 cluster with seed + neighbor; unrelated is not in subgraph.
	if len(result) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(result))
	}

	paths := clusterPaths(result[0])
	sort.Strings(paths)
	expected := []string{"kb/go/concurrency/channels.md", "kb/go/concurrency/goroutines.md"}
	if fmt.Sprintf("%v", paths) != fmt.Sprintf("%v", expected) {
		t.Errorf("expected %v, got %v", expected, paths)
	}
}

func TestScopedCluster_CategoryFallbackOnLouvainError(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	seed1 := factForLLM{File: "kb/go/concurrency/channels.md", Title: "channels", Body: "ch"}
	seed2 := factForLLM{File: "kb/go/concurrency/goroutines.md", Title: "goroutines", Body: "gr"}

	// Search for seed1's neighbors.
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/concurrency/goroutines.md"}}, Score: 80},
	}, nil)

	// Search for seed2's neighbors.
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/concurrency/channels.md"}}, Score: 80},
	}, nil)

	// ClusterFacts fails.
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{}, fmt.Errorf("no embeddings"))

	result, err := ScopedCluster([]factForLLM{seed1, seed2}, idx, 1.0, nil, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both seeds are in same category, so they should be grouped together.
	if len(result) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(result))
	}

	paths := clusterPaths(result[0])
	sort.Strings(paths)
	expected := []string{"kb/go/concurrency/channels.md", "kb/go/concurrency/goroutines.md"}
	if fmt.Sprintf("%v", paths) != fmt.Sprintf("%v", expected) {
		t.Errorf("expected %v, got %v", expected, paths)
	}
}

func TestScopedCluster_SingleFactClustersFiltered(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	seed := factForLLM{File: "kb/go/concurrency/channels.md", Title: "channels", Body: "ch"}

	// No neighbors found.
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return(nil, nil)

	// ClusterFacts returns a single-element cluster.
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/go/concurrency/channels.md"},
		},
	}, nil)

	result, err := ScopedCluster([]factForLLM{seed}, idx, 1.0, nil, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Single-fact cluster should be filtered out.
	if len(result) != 0 {
		t.Fatalf("expected 0 clusters, got %d", len(result))
	}
}

func TestScopedCluster_UnrelatedFactsExcluded(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	seed := factForLLM{File: "kb/go/concurrency/channels.md", Title: "channels", Body: "ch"}

	idx.EXPECT().Search(gomock.Any(), gomock.Any()).Return([]store.SearchResult{
		{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/concurrency/goroutines.md"}}, Score: 80},
	}, nil)

	// Louvain puts all three in one big cluster.
	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/go/concurrency/channels.md", "kb/go/concurrency/goroutines.md", "kb/rust/ownership/borrow.md"},
		},
	}, nil)

	result, err := ScopedCluster([]factForLLM{seed}, idx, 1.0, nil, "agent/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(result))
	}

	// Unrelated fact should NOT be in the cluster (not in subgraph).
	paths := clusterPaths(result[0])
	for _, p := range paths {
		if p == "kb/rust/ownership/borrow.md" {
			t.Error("unrelated fact should not be in cluster")
		}
	}
	if len(paths) != 2 {
		t.Errorf("expected 2 facts in cluster, got %d", len(paths))
	}
}

func TestCategoryDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"kb/go/concurrency/uuid.md", "kb/go/concurrency"},
		{"kb/python/async.md", "kb/python"},
		{"root.md", "."},
	}
	for _, tt := range tests {
		got := categoryDir(tt.input)
		if got != tt.want {
			t.Errorf("categoryDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGroupByCategory(t *testing.T) {
	facts := []factForLLM{
		{File: "kb/go/concurrency/a.md"},
		{File: "kb/go/concurrency/b.md"},
		{File: "kb/python/async/c.md"},
	}
	groups := groupByCategory(facts)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
}

func TestScopedCluster_ExcludeTypesPassedToSearch(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	seed := factForLLM{File: "kb/go/concurrency/channels.md", Title: "channels", Body: "ch"}

	// Expect Search to be called with ExcludeTypes populated.
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, q store.SearchQuery) ([]store.SearchResult, error) {
		if len(q.ExcludeTypes) != 1 || q.ExcludeTypes[0] != "hypothesis" {
			t.Errorf("expected ExcludeTypes=[hypothesis], got %v", q.ExcludeTypes)
		}
		return []store.SearchResult{
			{FactWithBody: store.FactWithBody{FactRecord: store.FactRecord{Path: "kb/go/concurrency/goroutines.md", Title: "Goroutines"}, Body: "goroutines"}, Score: 80},
		}, nil
	})

	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{
		Clusters: map[int][]string{
			0: {"kb/go/concurrency/channels.md", "kb/go/concurrency/goroutines.md"},
		},
	}, nil)

	result, err := ScopedCluster([]factForLLM{seed}, idx, 1.0, nil, "agent/test", "hypothesis")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(result))
	}
}

func TestScopedCluster_NoExcludeTypesByDefault(t *testing.T) {
	ctrl := gomock.NewController(t)
	idx := NewMockSearchIndex(ctrl)

	seed := factForLLM{File: "kb/go/concurrency/channels.md", Title: "channels", Body: "ch"}

	// Expect Search to be called without ExcludeTypes.
	idx.EXPECT().Search(gomock.Any(), gomock.Any()).DoAndReturn(func(branch string, q store.SearchQuery) ([]store.SearchResult, error) {
		if len(q.ExcludeTypes) != 0 {
			t.Errorf("expected empty ExcludeTypes, got %v", q.ExcludeTypes)
		}
		return nil, nil
	})

	idx.EXPECT().ClusterFacts(1.0, 2).Return(store.ClusterResult{}, fmt.Errorf("no embeddings"))

	_, _ = ScopedCluster([]factForLLM{seed}, idx, 1.0, nil, "agent/test")
}

// clusterPaths extracts file paths from a cluster for easier assertions.
func clusterPaths(cluster []factForLLM) []string {
	paths := make([]string, len(cluster))
	for i, f := range cluster {
		paths[i] = f.File
	}
	return paths
}
