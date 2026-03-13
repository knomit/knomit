package cluster

import (
	"fmt"
	"math"
	"sort"
)

// DistanceFunc computes the distance between two vectors.
type DistanceFunc func(a, b []float64) float64

// HDBSCANOptions configures the HDBSCAN clustering algorithm.
type HDBSCANOptions struct {
	MinClusterSize int          // minimum points to form a cluster (default 5)
	MinSamples     int          // core distance neighbors (default = MinClusterSize)
	Distance       DistanceFunc // distance function (default: Euclidean)
}

// HDBSCAN clusters points using the Hierarchical Density-Based Spatial
// Clustering of Applications with Noise algorithm.
// Returns a label per point; noise points receive label -1.
func HDBSCAN(points [][]float64, opts HDBSCANOptions) []int {
	n := len(points)
	if n == 0 {
		return nil
	}

	distFn := opts.Distance
	if distFn == nil {
		dim := len(points[0])
		distFn = func(a, b []float64) float64 {
			return euclidean(a, b, dim)
		}
	}

	// Compute pairwise distance matrix, then delegate.
	dist := make([][]float64, n)
	for i := 0; i < n; i++ {
		dist[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			d := distFn(points[i], points[j])
			dist[i][j] = d
			dist[j][i] = d
		}
	}

	return HDBSCANPrecomputed(dist, opts)
}

// HDBSCANPrecomputed clusters points given a precomputed NxN distance matrix.
// The Distance field in opts is ignored (distances are already computed).
// Returns a label per point; noise points receive label -1.
func HDBSCANPrecomputed(dist [][]float64, opts HDBSCANOptions) []int {
	n := len(dist)
	if n == 0 {
		return nil
	}

	if opts.MinClusterSize <= 0 {
		opts.MinClusterSize = 5
	}
	if opts.MinSamples <= 0 {
		opts.MinSamples = opts.MinClusterSize
	}
	// k for core distance must be < n
	k := opts.MinSamples
	if k >= n {
		k = n - 1
	}
	if k < 1 {
		k = 1
	}

	// -------------------------------------------------------------------------
	// Step 2: Compute core distances (k-th nearest neighbour distance).
	// -------------------------------------------------------------------------
	core := make([]float64, n)
	for i := 0; i < n; i++ {
		row := make([]float64, n-1)
		idx := 0
		for j := 0; j < n; j++ {
			if j == i {
				continue
			}
			row[idx] = dist[i][j]
			idx++
		}
		sort.Float64s(row)
		core[i] = row[k-1] // (k-1) because row is 0-indexed and k is count
	}

	// -------------------------------------------------------------------------
	// Step 3: Build mutual reachability graph and compute MST with Prim's.
	// mreach(a,b) = max(core(a), core(b), dist(a,b))
	// -------------------------------------------------------------------------
	type mstEdge struct {
		u, v   int
		weight float64
	}

	inMST := make([]bool, n)
	minWeight := make([]float64, n)
	parent := make([]int, n)
	for i := range minWeight {
		minWeight[i] = math.Inf(1)
		parent[i] = -1
	}
	minWeight[0] = 0.0

	mst := make([]mstEdge, 0, n-1)

	for iter := 0; iter < n; iter++ {
		// Pick vertex with minimum key not yet in MST
		u := -1
		for v := 0; v < n; v++ {
			if !inMST[v] && (u == -1 || minWeight[v] < minWeight[u]) {
				u = v
			}
		}
		inMST[u] = true
		if parent[u] != -1 {
			mst = append(mst, mstEdge{parent[u], u, minWeight[u]})
		}

		// Update neighbours
		for v := 0; v < n; v++ {
			if inMST[v] {
				continue
			}
			mreach := math.Max(core[u], math.Max(core[v], dist[u][v]))
			if mreach < minWeight[v] {
				minWeight[v] = mreach
				parent[v] = u
			}
		}
	}

	// -------------------------------------------------------------------------
	// Step 4: Build cluster hierarchy (single-linkage dendrogram) by processing
	// MST edges in ascending weight order.
	// -------------------------------------------------------------------------
	sort.Slice(mst, func(i, j int) bool {
		return mst[i].weight < mst[j].weight
	})

	// Union-Find for building hierarchy
	uf := newUnionFind(n)

	// Each node in the dendrogram: initially n leaf nodes (0..n-1),
	// then n-1 internal nodes (n..2n-2).
	// lambdaInv[node] = the distance (weight) at which this cluster was created.
	// size[node] = number of original points under this node.
	// children[node] = two children if internal node.
	type dendroNode struct {
		left, right int
		lambdaInv   float64 // = merge distance
		size        int
	}

	totalNodes := 2*n - 1
	dendro := make([]dendroNode, totalNodes)
	for i := 0; i < n; i++ {
		dendro[i] = dendroNode{left: -1, right: -1, size: 1}
	}

	nextID := n
	// Map from union-find root to dendro node ID
	nodeID := make([]int, n)
	for i := range nodeID {
		nodeID[i] = i
	}

	for _, e := range mst {
		ru := uf.find(e.u)
		rv := uf.find(e.v)
		if ru == rv {
			continue // shouldn't happen with a tree
		}
		// Merge
		newNode := nextID
		nextID++
		dendro[newNode] = dendroNode{
			left:      nodeID[ru],
			right:     nodeID[rv],
			lambdaInv: e.weight,
			size:      dendro[nodeID[ru]].size + dendro[nodeID[rv]].size,
		}
		uf.union(ru, rv)
		newRoot := uf.find(ru)
		nodeID[newRoot] = newNode
	}

	root := nextID - 1 // last created node is the root

	// -------------------------------------------------------------------------
	// Step 5: Extract flat clusters via excess of mass.
	// -------------------------------------------------------------------------
	// For each cluster node, compute stability = sum over points p of
	// (1/lambda_p_death - 1/lambda_birth) where lambda = 1/distance.
	// A node is selected if its own stability > sum of children stabilities.

	minSize := opts.MinClusterSize

	// stability[node] = excess of mass contribution
	stability := make([]float64, totalNodes)
	// birthLambda[node] = lambda at which this cluster was born (= 1/mergeWeight)
	birthLambda := make([]float64, totalNodes)

	// We traverse bottom-up. For leaf nodes (0..n-1), birthLambda is set by
	// their parent's merge weight. Process in reverse creation order.
	// First, set birthLambda for all internal nodes (they are born at their
	// parent's merge weight — so we set it when processing the parent).
	// We use a post-order traversal.

	// Set birthLambda for root's children
	var computeStability func(node int, birth float64)
	computeStability = func(node int, birth float64) {
		birthLambda[node] = birth
		d := dendro[node]
		if d.left == -1 {
			// Leaf: stability contribution is 0 (no children to compare)
			stability[node] = 0
			return
		}
		// Internal node: determine the lambda at this merge
		lambdaSplit := lambdaOf(d.lambdaInv)

		leftSize := dendro[d.left].size
		rightSize := dendro[d.right].size

		if leftSize >= minSize && rightSize >= minSize {
			// Both children are valid clusters: recurse on both.
			// The parent cluster's own stability = all N points contributed from
			// birth to the split (excess-of-mass formulation). This is NOT zeroed
			// because points were alive in this cluster from birth to lambdaSplit.
			computeStability(d.left, lambdaSplit)
			computeStability(d.right, lambdaSplit)
			stability[node] = float64(d.size) * (lambdaSplit - birth)
		} else if leftSize < minSize && rightSize < minSize {
			// Both children fall below minSize: they are noise, points fall into
			// this cluster. Add stability for all points.
			stability[node] = float64(d.size) * (lambdaSplit - birth)
		} else {
			// One child is valid, the other is noise.
			// The noise-side points fell out at lambdaSplit; the surviving child
			// inherits the cluster identity. The parent's own stability includes
			// the noise-side contribution plus the full cluster from birth to split.
			if leftSize >= minSize {
				// Right child is noise, left child is valid cluster
				computeStability(d.left, lambdaSplit)
				stability[node] = float64(d.size) * (lambdaSplit - birth)
			} else {
				// Left child is noise, right child is valid cluster
				computeStability(d.right, lambdaSplit)
				stability[node] = float64(d.size) * (lambdaSplit - birth)
			}
		}
	}

	computeStability(root, 0)

	// Now propagate stability up using the excess-of-mass criterion:
	// a cluster node is selected if its own accumulated stability >= sum of
	// selected children stabilities. When a node is selected, its children
	// are deselected (this node absorbs them).
	totalStability := make([]float64, totalNodes)
	isCluster := make([]bool, totalNodes)

	var propagate func(node int) float64
	propagate = func(node int) float64 {
		d := dendro[node]
		if d.left == -1 {
			totalStability[node] = 0
			return 0
		}
		leftSize := dendro[d.left].size
		rightSize := dendro[d.right].size

		childStab := 0.0
		if leftSize >= minSize {
			childStab += propagate(d.left)
		}
		if rightSize >= minSize {
			childStab += propagate(d.right)
		}

		// EOM decision: if this node's own stability >= sum of children
		// stabilities, select this node (it is a better cluster boundary).
		// Otherwise, pass children up as the selected clusters.
		if stability[node] >= childStab {
			// Select this node, deselect children
			isCluster[node] = true
			totalStability[node] = stability[node]
		} else {
			// Keep children
			isCluster[node] = false
			totalStability[node] = childStab
		}
		return totalStability[node]
	}

	propagate(root)
	// Root is always considered a cluster if nothing else is selected
	if !isCluster[root] {
		// Check if any children are selected; if not, select root
		hasChild := false
		for i := n; i < totalNodes; i++ {
			if isCluster[i] && i != root {
				hasChild = true
				break
			}
		}
		if !hasChild {
			isCluster[root] = true
		}
	}

	// -------------------------------------------------------------------------
	// Step 6: Assign points to clusters.
	// -------------------------------------------------------------------------
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}

	clusterCounter := 0

	var assignLabels func(node int, clusterLabel int)
	assignLabels = func(node int, clusterLabel int) {
		d := dendro[node]
		if d.left == -1 {
			// Leaf: assign this point
			labels[node] = clusterLabel
			return
		}

		if isCluster[node] && clusterLabel == -1 {
			// This node is a new cluster
			cl := clusterCounter
			clusterCounter++
			// Assign all points under this node
			assignLabels(d.left, cl)
			assignLabels(d.right, cl)
			return
		}

		leftSize := dendro[d.left].size
		rightSize := dendro[d.right].size

		// Pass current cluster label down
		if leftSize >= minSize {
			if isCluster[d.left] && clusterLabel == -1 {
				// Will be handled by recursive call
				assignLabels(d.left, -1)
			} else {
				assignLabels(d.left, clusterLabel)
			}
		} else {
			// Small side: noise (label = -1) if no current cluster
			assignLabels(d.left, clusterLabel)
		}
		if rightSize >= minSize {
			if isCluster[d.right] && clusterLabel == -1 {
				assignLabels(d.right, -1)
			} else {
				assignLabels(d.right, clusterLabel)
			}
		} else {
			assignLabels(d.right, clusterLabel)
		}
	}

	assignLabels(root, -1)

	// Remap cluster labels to be contiguous from 0
	labelMap := make(map[int]int)
	nextLabel := 0
	for i, l := range labels {
		if l == -1 {
			continue
		}
		if _, ok := labelMap[l]; !ok {
			labelMap[l] = nextLabel
			nextLabel++
		}
		labels[i] = labelMap[l]
	}

	return labels
}

func lambdaOf(dist float64) float64 {
	if dist == 0 {
		return math.Inf(1)
	}
	return 1.0 / dist
}

// -------------------------------------------------------------------------
// Union-Find (path-compressed, union by rank)
// -------------------------------------------------------------------------

type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	uf := &unionFind{
		parent: make([]int, n),
		rank:   make([]int, n),
	}
	for i := range uf.parent {
		uf.parent[i] = i
	}
	return uf
}

func (uf *unionFind) find(x int) int {
	for uf.parent[x] != x {
		uf.parent[x] = uf.parent[uf.parent[x]] // path compression
		x = uf.parent[x]
	}
	return x
}

func (uf *unionFind) union(a, b int) {
	ra, rb := uf.find(a), uf.find(b)
	if ra == rb {
		return
	}
	if uf.rank[ra] < uf.rank[rb] {
		uf.parent[ra] = rb
	} else if uf.rank[ra] > uf.rank[rb] {
		uf.parent[rb] = ra
	} else {
		uf.parent[rb] = ra
		uf.rank[ra]++
	}
}

// -------------------------------------------------------------------------
// FCA metadata splitting
// -------------------------------------------------------------------------

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

// -------------------------------------------------------------------------
// ClusterFacts: top-level entry point
// -------------------------------------------------------------------------

// ClusterOptions configures the ClusterFacts pipeline.
type ClusterOptions struct {
	UMAPDimensions int // target dimensions for UMAP (default 5)
	MinClusterSize int // minimum points per cluster (default 3)
}

// ClusterResult holds the output of ClusterFacts.
type ClusterResult struct {
	Clusters map[int][]int // cluster label → fact indices
	Noise    []int         // fact indices classified as noise
}

// ClusterFacts clusters fact embeddings using UMAP + HDBSCAN + FCA metadata split.
// embeddings[i] is the embedding for facts[i] (or metas[i]).
func ClusterFacts(embeddings [][]float32, metas []FactMeta, opts ClusterOptions) (ClusterResult, error) {
	n := len(embeddings)
	if n == 0 {
		return ClusterResult{Clusters: map[int][]int{}, Noise: nil}, nil
	}
	if len(metas) != n {
		return ClusterResult{}, fmt.Errorf("cluster: embeddings and metas length mismatch: %d vs %d", n, len(metas))
	}

	if opts.UMAPDimensions <= 0 {
		opts.UMAPDimensions = 5
	}
	if opts.MinClusterSize <= 0 {
		opts.MinClusterSize = 3
	}

	// 1. float32 → float64
	vecs := make([][]float64, n)
	for i, emb := range embeddings {
		v := make([]float64, len(emb))
		for j, x := range emb {
			v[j] = float64(x)
		}
		vecs[i] = v
	}

	// 2. UMAP dimensionality reduction
	nNeighbors := 15
	if nNeighbors >= n {
		nNeighbors = n - 1
	}
	if nNeighbors < 1 {
		nNeighbors = 1
	}
	reduced, err := UMAP(vecs, UMAPOptions{
		NComponents: opts.UMAPDimensions,
		NNeighbors:  nNeighbors,
		MinDist:     0.1,
		Seed:        42,
	})
	if err != nil {
		return ClusterResult{}, fmt.Errorf("cluster: UMAP failed: %w", err)
	}

	// 3. HDBSCAN
	hdbscanLabels := HDBSCAN(reduced, HDBSCANOptions{
		MinClusterSize: opts.MinClusterSize,
	})

	// 4. FCA metadata split
	finalLabels := SplitByMetadata(metas, hdbscanLabels, opts.MinClusterSize)

	// 5. Build ClusterResult
	clusters := make(map[int][]int)
	noise := []int{}
	for i, l := range finalLabels {
		if l == -1 {
			noise = append(noise, i)
		} else {
			clusters[l] = append(clusters[l], i)
		}
	}

	return ClusterResult{Clusters: clusters, Noise: noise}, nil
}
