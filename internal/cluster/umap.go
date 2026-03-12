// Package cluster implements UMAP (Uniform Manifold Approximation and
// Projection) dimensionality reduction. Reduces high-dimensional embedding
// vectors to a lower-dimensional space while preserving local neighborhood
// structure. Ported from umap-js (https://arxiv.org/abs/1802.03426).
package cluster

import (
	"fmt"
	"math"
	"math/rand"
)

// UMAPOptions configures the UMAP dimensionality reduction algorithm.
type UMAPOptions struct {
	NComponents int     // target dimensions, e.g. 5
	NNeighbors  int     // k nearest neighbours, e.g. 10 or 15
	MinDist     float64 // minimum distance in low-dim space, e.g. 0.1
	Seed        int64   // random seed (0 = use default source)
}

// UMAP reduces high-dimensional vectors to NComponents dimensions using the
// Uniform Manifold Approximation and Projection algorithm.
// Ported from umap-js, which implements https://arxiv.org/abs/1802.03426.
//
// The algorithm proceeds in six phases:
//  1. Build a k-nearest-neighbor graph (brute-force Euclidean distance).
//  2. Smooth kNN distances — binary-search for per-point bandwidth (sigma).
//  3. Construct a fuzzy simplicial set by symmetrizing the kNN graph.
//  4. Initialize the low-dimensional embedding (random layout).
//  5. Precompute smooth curve parameters (a, b) from minDist.
//  6. Optimize the embedding via stochastic gradient descent (SGD).
func UMAP(vectors [][]float64, opts UMAPOptions) ([][]float64, error) {
	n := len(vectors)
	if n == 0 {
		return nil, fmt.Errorf("umap: no input vectors")
	}
	if opts.NComponents <= 0 {
		opts.NComponents = 2
	}
	if opts.NNeighbors <= 0 {
		opts.NNeighbors = 15
	}
	if opts.NNeighbors >= n {
		opts.NNeighbors = n - 1
	}
	if opts.NNeighbors < 1 {
		return nil, fmt.Errorf("umap: need at least 2 vectors, got %d", n)
	}
	if opts.MinDist <= 0 {
		opts.MinDist = 0.1
	}

	var rng *rand.Rand
	if opts.Seed != 0 {
		rng = rand.New(rand.NewSource(opts.Seed))
	} else {
		rng = rand.New(rand.NewSource(42))
	}

	dim := len(vectors[0])

	// -------------------------------------------------------------------------
	// Step 1: k-nearest neighbours (brute-force Euclidean)
	// -------------------------------------------------------------------------
	// Brute-force is O(n²·d) which is acceptable for our use case (hundreds to
	// low thousands of facts). For larger datasets, an approximate NN index
	// (e.g. random-projection trees) would be needed.
	knnIndices := make([][]int, n)
	knnDists := make([][]float64, n)
	k := opts.NNeighbors

	for i := 0; i < n; i++ {
		// Find k nearest neighbours of vectors[i]
		all := make([]knnEntry, 0, n-1)
		for j := 0; j < n; j++ {
			if j == i {
				continue
			}
			d := euclidean(vectors[i], vectors[j], dim)
			all = append(all, knnEntry{d, j})
		}
		// Partial sort: find k smallest
		knnPartialSort(all, k)

		knnIndices[i] = make([]int, k)
		knnDists[i] = make([]float64, k)
		for m := 0; m < k; m++ {
			knnIndices[i][m] = all[m].idx
			knnDists[i][m] = all[m].dist
		}
	}

	// -------------------------------------------------------------------------
	// Step 2: Smooth kNN distances — find rho and sigma per point
	// -------------------------------------------------------------------------
	// Binary search for sigma (bandwidth) per point such that the sum of
	// neighbor membership strengths equals log2(k). rho is the distance to the
	// nearest neighbor, ensuring at least one neighbor has membership 1. This
	// creates an adaptive kernel: points in sparse regions get wider bandwidths.
	rhos := make([]float64, n)   // distance to nearest neighbour
	sigmas := make([]float64, n) // bandwidth

	target := math.Log2(float64(k))
	const iterations = 64
	const bandwidth = 1.0

	for i := 0; i < n; i++ {
		rhos[i] = knnDists[i][0]

		lo, hi := 0.0, 1e10
		mid := 1.0

		for iter := 0; iter < iterations; iter++ {
			sum := 0.0
			for m := 0; m < k; m++ {
				d := knnDists[i][m] - rhos[i]
				if d <= 0 {
					sum += 1.0
				} else {
					sum += math.Exp(-d / mid)
				}
			}
			if math.Abs(sum-target) < 1e-5 {
				break
			}
			if sum > target {
				hi = mid
				mid = (lo + hi) / 2.0
			} else {
				lo = mid
				if hi == 1e10 {
					mid *= 2.0
				} else {
					mid = (lo + hi) / 2.0
				}
			}
		}
		sigmas[i] = mid * bandwidth
	}

	// -------------------------------------------------------------------------
	// Step 3: Build fuzzy simplicial set (symmetrized membership strengths)
	// -------------------------------------------------------------------------
	// Symmetrize the directed kNN graph into an undirected fuzzy graph.
	// The formula w_ij = v_ij + v_ji - v_ij*v_ji is the probabilistic t-conorm
	// (fuzzy OR), meaning: the probability that i and j are neighbors according
	// to either i's or j's perspective.
	// Store as sparse COO: row, col, val
	type edge struct {
		i, j int
		w    float64
	}
	edgeMap := make(map[[2]int]float64)

	for i := 0; i < n; i++ {
		for m := 0; m < k; m++ {
			j := knnIndices[i][m]
			d := knnDists[i][m]
			var v float64
			diff := d - rhos[i]
			if diff <= 0 {
				v = 1.0
			} else if sigmas[i] > 0 {
				v = math.Exp(-diff / sigmas[i])
			}
			key := [2]int{i, j}
			edgeMap[key] = v
		}
	}

	// Symmetrize: w_ij = v_ij + v_ji - v_ij * v_ji
	edges := make([]edge, 0, len(edgeMap))
	seen := make(map[[2]int]bool)
	for key, vij := range edgeMap {
		i, j := key[0], key[1]
		revKey := [2]int{j, i}
		if seen[revKey] {
			continue
		}
		vji := edgeMap[revKey]
		w := vij + vji - vij*vji
		if w > 0 {
			edges = append(edges, edge{i, j, w})
		}
		seen[key] = true
		seen[revKey] = true
	}

	// -------------------------------------------------------------------------
	// Step 4: Initialise low-dimensional embedding (random in [-10, 10])
	// -------------------------------------------------------------------------
	// Random initialization in [-10, 10]. Spectral initialization (PCA) would
	// converge faster but adds complexity; random works well enough with 200
	// SGD epochs.
	embedding := make([][]float64, n)
	for i := range embedding {
		embedding[i] = make([]float64, opts.NComponents)
		for d := 0; d < opts.NComponents; d++ {
			embedding[i][d] = (rng.Float64()*2 - 1) * 10.0
		}
	}

	// -------------------------------------------------------------------------
	// Step 5: Precompute curve parameters a, b from minDist
	// -------------------------------------------------------------------------
	// Precompute smooth curve parameters a, b that model the desired distance
	// distribution in low-dimensional space. The curve 1/(1 + a*d^(2b)) maps
	// high-dim distances to membership strengths. minDist controls how tightly
	// points can pack: smaller = denser clusters.
	a, b := findAB(opts.MinDist)

	// -------------------------------------------------------------------------
	// Step 6: SGD optimisation
	// -------------------------------------------------------------------------
	nEpochs := 200
	lr := 1.0
	nNeg := 5

	// Edges are sampled proportional to their weight per epoch — high-weight
	// edges are optimized more frequently.
	// Build epoch weights: sample edges proportional to weight
	totalWeight := 0.0
	for _, e := range edges {
		totalWeight += e.w
	}
	epochsPerEdge := make([]float64, len(edges))
	for idx, e := range edges {
		epochsPerEdge[idx] = e.w / totalWeight * float64(nEpochs)
	}

	// Track per-edge epoch counter
	epochsPerSample := make([]float64, len(edges))
	copy(epochsPerSample, epochsPerEdge)
	epochNextSample := make([]float64, len(edges))
	copy(epochNextSample, epochsPerEdge)
	epochNextNegativeSample := make([]float64, len(edges))
	negSampleRate := float64(nNeg)
	for idx := range epochNextNegativeSample {
		epochNextNegativeSample[idx] = epochsPerSample[idx] / negSampleRate
	}

	// Clip gradients to [-4, 4] to prevent divergence from outliers.
	clip := func(x float64) float64 {
		if x > 4.0 {
			return 4.0
		}
		if x < -4.0 {
			return -4.0
		}
		return x
	}

	for epoch := 0; epoch < nEpochs; epoch++ {
		// Linear decay from 1.0 to 0.0 over epochs (simulated annealing).
		alpha := lr * (1.0 - float64(epoch)/float64(nEpochs))

		for idx, e := range edges {
			if epochNextSample[idx] > float64(epoch) {
				continue
			}

			current := embedding[e.i]
			other := embedding[e.j]

			distSq := 0.0
			for d := 0; d < opts.NComponents; d++ {
				diff := current[d] - other[d]
				distSq += diff * diff
			}
			if distSq == 0 {
				distSq = 1e-10
			}

			// Attraction gradient: move connected points closer. This is the
			// gradient of cross-entropy loss for the attractive force.
			gradCoeff := -2.0 * a * b * math.Pow(distSq, b-1.0)
			gradCoeff /= a*math.Pow(distSq, b) + 1.0

			for d := 0; d < opts.NComponents; d++ {
				diff := current[d] - other[d]
				g := clip(gradCoeff * diff)
				current[d] += alpha * g
				other[d] -= alpha * g
			}
			embedding[e.i] = current
			embedding[e.j] = other

			epochNextSample[idx] += epochsPerSample[idx]

			// Negative sampling
			nNegSamples := 0
			for epochNextNegativeSample[idx] <= float64(epoch) {
				nNegSamples++
				epochNextNegativeSample[idx] += epochsPerSample[idx] / negSampleRate
			}

			for nn := 0; nn < nNegSamples; nn++ {
				k2 := rng.Intn(n)
				if k2 == e.i {
					continue
				}
				neg := embedding[k2]
				dSq := 0.0
				for d := 0; d < opts.NComponents; d++ {
					diff := current[d] - neg[d]
					dSq += diff * diff
				}
				if dSq == 0 {
					dSq = 1e-10
				}

				// Repulsion gradient: push random non-neighbors apart. Negative
				// sampling approximates the repulsive force from all non-edges,
				// similar to word2vec's approach.
				gradCoeff2 := 2.0 * b
				gradCoeff2 /= (0.001+dSq)*(a*math.Pow(dSq, b)+1.0)

				for d := 0; d < opts.NComponents; d++ {
					diff := current[d] - neg[d]
					g := clip(gradCoeff2 * diff)
					current[d] += alpha * g
				}
				embedding[e.i] = current
			}
		}
	}

	return embedding, nil
}

// knnEntry holds a neighbour index and its distance.
type knnEntry struct {
	dist float64
	idx  int
}

// euclidean computes the Euclidean distance between two vectors of length dim.
func euclidean(a, b []float64, dim int) float64 {
	sum := 0.0
	for i := 0; i < dim; i++ {
		d := a[i] - b[i]
		sum += d * d
	}
	return math.Sqrt(sum)
}

// CosineDistance computes 1 - cosine_similarity(a, b).
// This metric works well in high-dimensional spaces where Euclidean distance
// suffers from the curse of dimensionality.
func CosineDistance(a, b []float64) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 1.0
	}
	sim := dot / (math.Sqrt(normA) * math.Sqrt(normB))
	if sim > 1.0 {
		sim = 1.0
	}
	if sim < -1.0 {
		sim = -1.0
	}
	return 1.0 - sim
}

// knnPartialSort reorders all[:k] so that all[:k] are the k smallest elements
// (by dist) in sorted order. Uses selection sort, fine for small k (≤15).
func knnPartialSort(all []knnEntry, k int) {
	if k >= len(all) {
		return
	}
	for i := 0; i < k; i++ {
		minIdx := i
		for j := i + 1; j < len(all); j++ {
			if all[j].dist < all[minIdx].dist {
				minIdx = j
			}
		}
		all[i], all[minIdx] = all[minIdx], all[i]
	}
}

// findAB computes the UMAP curve parameters a, b from minDist using a
// least-squares fit on the smooth approximation of the distance function.
// This matches the approach in umap-js.
//
// Common minDist values are precomputed to avoid the expensive gradient descent.
//
// Results are cached for known minDist values to avoid the expensive
// 10,000-iteration gradient descent on every call (~3M float ops).
func findAB(minDist float64) (float64, float64) {
	// Precomputed values for common minDist (spread=1.0).
	switch minDist {
	case 0.1:
		return 1.576636002939383, 0.894641445561886
	}
	return findABSlow(minDist)
}

// findABSlow computes a, b via gradient descent (expensive).
// Fits the smooth curve 1/(1 + a*x^(2b)) to a step function that is 1 for
// x < minDist and decays exponentially after, using 10,000 iterations of
// gradient descent over 300 sample points.
func findABSlow(minDist float64) (float64, float64) {
	spread := 1.0
	xv := make([]float64, 300)
	yv := make([]float64, 300)
	for i := range xv {
		x := float64(i) * spread * 3.0 / 300.0
		xv[i] = x
		if x < minDist {
			yv[i] = 1.0
		} else {
			yv[i] = math.Exp(-(x - minDist) / spread)
		}
	}

	a, b := 1.0, 1.0
	lr := 0.001
	for iter := 0; iter < 10000; iter++ {
		dA, dB := 0.0, 0.0
		for i := range xv {
			x2b := math.Pow(xv[i]*xv[i], b)
			denom := 1.0 + a*x2b
			pred := 1.0 / denom
			err := pred - yv[i]
			dA += err * (-x2b / (denom * denom))
			if xv[i] > 0 {
				lnx2 := math.Log(xv[i] * xv[i])
				dB += err * (-a * x2b * lnx2 / (denom * denom))
			}
		}
		a -= lr * dA
		b -= lr * dB
		if a < 0.01 {
			a = 0.01
		}
		if b < 0.01 {
			b = 0.01
		}
	}
	return a, b
}
