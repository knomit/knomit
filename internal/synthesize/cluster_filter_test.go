package synthesize

import "testing"

// TestFilterSmallClusters_HonoursMinCommunitySize regresses PR #70 review
// finding #4: filterSmallClusters hardcoded "len(c) > 1", so the category
// fallback paths ignored the configurable min_community_size knob. It must now
// drop clusters smaller than the configured minimum.
func TestFilterSmallClusters_HonoursMinCommunitySize(t *testing.T) {
	clusters := [][]factForLLM{
		{{File: "a"}},                           // size 1
		{{File: "b"}, {File: "c"}},              // size 2
		{{File: "d"}, {File: "e"}, {File: "f"}}, // size 3
	}

	cases := []struct {
		min  int
		want int // number of clusters surviving
	}{
		{1, 3}, // keep everything, including singletons
		{2, 2}, // drop the singleton (the previous hardcoded behaviour)
		{3, 1}, // drop sizes 1 and 2
		{4, 0}, // nothing large enough
	}
	for _, tc := range cases {
		got := filterSmallClusters(clusters, tc.min)
		if len(got) != tc.want {
			t.Fatalf("min=%d: kept %d clusters, want %d", tc.min, len(got), tc.want)
		}
	}
}
