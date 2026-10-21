package synthesize

// intraCommunityEdges returns a star of SIMILAR_TO edges within each community
// (first member linked to the rest) and no cross-community edge, so Louvain
// recovers exactly the given partition. Singleton communities contribute no
// edge.
func intraCommunityEdges(comms [][]string) [][2]string {
	var edges [][2]string
	for _, c := range comms {
		for i := 1; i < len(c); i++ {
			edges = append(edges, [2]string{c[0], c[i]})
		}
	}
	return edges
}
