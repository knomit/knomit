package synthesize

import (
	"sort"
	"strings"
	"testing"
)

// partitionKey renders a community partition as a stable, order-independent
// string so tests can assert on grouping without depending on community order
// or intra-community ordering.
func partitionKey(communities [][]string) string {
	groups := make([]string, 0, len(communities))
	for _, c := range communities {
		members := append([]string(nil), c...)
		sort.Strings(members)
		groups = append(groups, strings.Join(members, ","))
	}
	sort.Strings(groups)
	return strings.Join(groups, " | ")
}

func TestLouvainCommunities_SeparatesDisconnectedClusters(t *testing.T) {
	// Two triangles with no edge between them must fall into two communities.
	paths := []string{"a", "b", "c", "d", "e", "f"}
	edges := [][2]string{
		{"a", "b"}, {"b", "c"}, {"a", "c"},
		{"d", "e"}, {"e", "f"}, {"d", "f"},
	}

	got := louvainCommunities(paths, edges, 1.0)

	want := "a,b,c | d,e,f"
	if partitionKey(got) != want {
		t.Fatalf("partition = %q, want %q", partitionKey(got), want)
	}
}

func TestLouvainCommunities_IsolatedNodeIsOwnCommunity(t *testing.T) {
	// A node present in paths but absent from edges must survive as a singleton,
	// never silently dropped.
	paths := []string{"a", "b", "lonely"}
	edges := [][2]string{{"a", "b"}}

	got := louvainCommunities(paths, edges, 1.0)

	var found bool
	for _, c := range got {
		if len(c) == 1 && c[0] == "lonely" {
			found = true
		}
	}
	if !found {
		t.Fatalf("isolated node 'lonely' missing from partition %v", got)
	}
}
