package cluster

import (
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
//
// The algorithm proceeds in six steps:
//  1. (Caller provides the distance matrix.)
//  2. Compute core distances — the k-th nearest neighbor distance for each point.
//  3. Build a mutual reachability graph and extract its minimum spanning tree
//     using Prim's algorithm. Mutual reachability distance between points a and b
//     is max(core(a), core(b), dist(a,b)), which inflates distances in sparse
//     regions so that only genuinely dense areas form clusters.
//  4. Build a single-linkage dendrogram by processing MST edges in ascending
//     weight order, merging clusters via union-find.
//  5. Extract flat clusters using the "excess of mass" (EOM) criterion:
//     each cluster accumulates stability proportional to how long its points
//     persist (measured in lambda = 1/distance space). A parent cluster is
//     selected over its children when its own stability exceeds the sum of
//     its children's stabilities — meaning the single large cluster is a
//     better explanation than the two sub-clusters.
//  6. Assign each point to its selected cluster (or noise = -1).
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
	//
	// Mutual reachability distance: mreach(a,b) = max(core(a), core(b), dist(a,b)).
	// This inflates short edges in low-density regions, ensuring that only
	// points in genuinely dense neighborhoods merge early.
	//
	// Prim's algorithm runs in O(n^2) which is optimal here: we already have
	// the O(n^2) distance matrix in memory, so there is no benefit to a
	// heap-based O(E log V) approach.
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

	// Dendrogram node fields:
	//   left, right — child node indices (-1 for leaf nodes)
	//   lambdaInv   — the merge distance at which this node was created
	//                  (i.e. the MST edge weight that caused the merge)
	//   size        — total number of original points below this node
	//
	// Nodes 0..n-1 are leaves (one per input point).
	// Nodes n..2n-2 are internal merge nodes, created in merge order.
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
	// Step 5: Extract flat clusters via "excess of mass" (EOM).
	//
	// Each cluster node accumulates stability = sum over its points of
	// (lambda_death - lambda_birth), where lambda = 1/distance. A point's
	// lambda_death is the lambda at which it separates from the cluster
	// (either by a split or by falling below minClusterSize). The EOM
	// criterion selects a parent cluster over its children when the parent's
	// own stability exceeds the combined stability of its children — meaning
	// the single cluster is a better density-based explanation than two
	// sub-clusters.
	// -------------------------------------------------------------------------
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

// lambdaOf converts a distance to lambda-space (1/distance).
// Returns +Inf for zero distance.
func lambdaOf(dist float64) float64 {
	if dist == 0 {
		return math.Inf(1)
	}
	return 1.0 / dist
}
