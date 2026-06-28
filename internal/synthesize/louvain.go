package synthesize

import (
	"math/rand/v2"

	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

// louvainSeed fixes the PRNG that Louvain uses to randomise node visitation
// order, so clustering the same subgraph twice yields the same partition. The
// value is arbitrary; only its stability matters.
const louvainSeed = 0x6b6e6f6d6974 // "knomit"

// louvainCommunities partitions paths into communities by running Louvain
// community detection over the undirected graph induced by edges. Every path is
// represented even if it has no edges (it forms a singleton community), so no
// input fact is ever silently dropped. Edge endpoints not present in paths are
// still admitted to the graph, but in practice callers pass edges drawn from the
// same path set.
//
// resolution is gonum's Louvain resolution: higher values yield more, smaller
// communities. It mirrors the parameter the old graph-wide louvain() took.
func louvainCommunities(paths []string, edges [][2]string, resolution float64) [][]string {
	if len(paths) == 0 {
		return nil
	}

	g := simple.NewUndirectedGraph()

	// Assign each distinct path a stable int64 node id and back-map for results.
	idByPath := make(map[string]int64, len(paths))
	pathByID := make(map[int64]string, len(paths))
	add := func(p string) int64 {
		if id, ok := idByPath[p]; ok {
			return id
		}
		id := int64(len(idByPath))
		idByPath[p] = id
		pathByID[id] = p
		g.AddNode(simple.Node(id))
		return id
	}
	for _, p := range paths {
		add(p)
	}
	for _, e := range edges {
		a, b := add(e[0]), add(e[1])
		if a == b {
			continue // ignore self-loops
		}
		g.SetEdge(simple.Edge{F: simple.Node(a), T: simple.Node(b)})
	}

	src := rand.NewPCG(louvainSeed, louvainSeed)
	reduced := community.Modularize(g, resolution, src)

	var out [][]string
	for _, members := range reduced.Communities() {
		group := make([]string, 0, len(members))
		for _, n := range members {
			group = append(group, pathByID[n.ID()])
		}
		if len(group) > 0 {
			out = append(out, group)
		}
	}
	return out
}
