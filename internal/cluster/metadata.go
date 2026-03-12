// Post-processing: splits HDBSCAN clusters into finer-grained groups
// based on shared domain/entity tags using connected-component analysis
// (Formal Concept Analysis-inspired).
package cluster

// FactMeta holds the metadata fields used for FCA-based cluster splitting.
type FactMeta struct {
	Domain   []string
	Entities []string
}

// SplitByMetadata splits clusters into connected components where two facts
// are connected if they share at least one domain or entity tag.
// Components smaller than minSize are reclassified as noise (-1).
// Input labels are the HDBSCAN output; only non-noise points are processed.
// Returns a new label slice with the same indexing as labels.
//
// Tag key format: "d:" prefix for domain tags, "e:" prefix for entity tags.
// The union-find operates on local indices (0..m-1) mapped from global fact
// indices within each cluster, keeping memory proportional to cluster size.
// Two facts are in the same connected component if they share any tag.
func SplitByMetadata(facts []FactMeta, labels []int, minSize int) []int {
	n := len(facts)
	result := make([]int, n)
	for i, l := range labels {
		result[i] = l
	}

	// Group indices by cluster label
	clusterIndices := make(map[int][]int)
	for i, l := range labels {
		if l == -1 {
			continue
		}
		clusterIndices[l] = append(clusterIndices[l], i)
	}

	nextLabel := 0
	// Find the max existing label to avoid collisions
	for l := range clusterIndices {
		if l >= nextLabel {
			nextLabel = l + 1
		}
	}

	for _, indices := range clusterIndices {
		m := len(indices)
		if m == 0 {
			continue
		}

		// Union-Find over local indices (0..m-1)
		uf := newUnionFind(m)

		// Build tag -> local indices map
		tagToLocal := make(map[string][]int)
		for li, gi := range indices {
			fact := facts[gi]
			for _, d := range fact.Domain {
				key := "d:" + d
				tagToLocal[key] = append(tagToLocal[key], li)
			}
			for _, e := range fact.Entities {
				key := "e:" + e
				tagToLocal[key] = append(tagToLocal[key], li)
			}
		}

		if len(tagToLocal) == 0 {
			// No tags: one component, keep original label
			// (assign a new label for all)
			lbl := nextLabel
			nextLabel++
			for _, gi := range indices {
				result[gi] = lbl
			}
			continue
		}

		// Union facts sharing a tag
		for _, locals := range tagToLocal {
			for i := 1; i < len(locals); i++ {
				uf.union(locals[0], locals[i])
			}
		}

		// Group by root
		components := make(map[int][]int)
		for li, gi := range indices {
			root := uf.find(li)
			components[root] = append(components[root], gi)
		}

		// If only one component, keep it (assign new label)
		if len(components) == 1 {
			lbl := nextLabel
			nextLabel++
			for _, gi := range indices {
				result[gi] = lbl
			}
			continue
		}

		// Multiple components: assign new labels, noise if too small
		for _, members := range components {
			if len(members) >= minSize {
				lbl := nextLabel
				nextLabel++
				for _, gi := range members {
					result[gi] = lbl
				}
			} else {
				for _, gi := range members {
					result[gi] = -1
				}
			}
		}
	}

	// Compact labels to 0..k-1
	labelMap := make(map[int]int)
	compact := 0
	final := make([]int, n)
	for i, l := range result {
		if l == -1 {
			final[i] = -1
			continue
		}
		if _, ok := labelMap[l]; !ok {
			labelMap[l] = compact
			compact++
		}
		final[i] = labelMap[l]
	}
	return final
}
