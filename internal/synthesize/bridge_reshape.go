package synthesize

import (
	"sort"

	"knomit/internal/store"
)

// reshapeCohesiveSubset extracts a tight cross-community cohesive subset from
// an oversized bridge token-group. It is pure over an already-fetched
// SimilarityGraph: no index, no context, fully deterministic.
//
// Algorithm:
//  1. SEED: find the connected cross-community pair (a,b) that maximises the
//     count of shared member-neighbours. Tie-break: lex-smallest sorted pair.
//     If no such pair exists, return nil (caller drops the token).
//  2. GROW: greedily add the candidate c that contributes the most edges to
//     the current subset. Accept c only if g.Density(subset∪{c}) >= cohFloor.
//     Stop when no candidate meets the density floor or adds any edge, or when
//     maxMembers is reached.
//  3. Return subset sorted by path; size in [2, maxMembers].
func reshapeCohesiveSubset(
	members []string,
	g store.SimilarityGraph,
	clusterOf map[string]int,
	cohFloor float64,
	maxMembers int,
) []string {
	// Work on a deterministic copy: sort members to eliminate slice-order
	// dependence. All selection loops below also sort their candidate sets so
	// map-iteration order never leaks into results.
	sorted := make([]string, len(members))
	copy(sorted, members)
	sort.Strings(sorted)

	// ── Step 1: SEED ──────────────────────────────────────────────────────────
	// isCrossCommunity: a and b are in different communities iff it is NOT the
	// case that both are in clusterOf with the same id.
	isCrossCommunity := func(a, b string) bool {
		ia, aKnown := clusterOf[a]
		ib, bKnown := clusterOf[b]
		if aKnown && bKnown {
			return ia != ib
		}
		// At least one is absent: two absent paths are distinct sentinels,
		// one absent + one present is always cross-community.
		return true
	}

	bestSeedA, bestSeedB := "", ""
	bestShared := -1

	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			a, b := sorted[i], sorted[j]
			if !g.Connected(a, b) {
				continue
			}
			if !isCrossCommunity(a, b) {
				continue
			}
			// Count shared neighbours within members.
			shared := 0
			for _, m := range sorted {
				if m != a && m != b && g.Connected(a, m) && g.Connected(b, m) {
					shared++
				}
			}
			// Prefer more shared neighbours; tie-break: lex-smallest pair
			// (already a<b because sorted[i]<sorted[j]).
			if shared > bestShared ||
				(shared == bestShared && (a < bestSeedA || (a == bestSeedA && b < bestSeedB))) {
				bestShared = shared
				bestSeedA, bestSeedB = a, b
			}
		}
	}

	if bestSeedA == "" {
		return nil // no cross-community connected pair
	}

	// ── Step 2: GROW ──────────────────────────────────────────────────────────
	inSubset := make(map[string]bool, maxMembers)
	inSubset[bestSeedA] = true
	inSubset[bestSeedB] = true
	subset := []string{bestSeedA, bestSeedB}

	for len(subset) < maxMembers {
		// Collect candidates: members not yet in subset, sorted for determinism.
		var candidates []string
		for _, m := range sorted {
			if !inSubset[m] {
				candidates = append(candidates, m)
			}
		}
		if len(candidates) == 0 {
			break
		}

		// Pick the candidate that adds the most edges to the current subset.
		// Tie-break: lex-smallest path (already sorted, so first winner wins).
		bestCand := ""
		bestEdges := -1
		for _, c := range candidates {
			edges := 0
			for _, s := range subset {
				if g.Connected(c, s) {
					edges++
				}
			}
			if edges > bestEdges {
				bestEdges = edges
				bestCand = c
			}
			// No need for explicit tie-break loop: candidates is sorted, and
			// strictly-greater-only update means first (lex-smallest) wins ties.
		}

		// Stop if best candidate adds 0 edges.
		if bestEdges == 0 {
			break
		}

		// Accept only if density stays at or above cohFloor.
		trial := append(subset, bestCand)
		if g.Density(trial) < cohFloor {
			break
		}

		inSubset[bestCand] = true
		subset = trial
	}

	// ── Step 3: return path-sorted ────────────────────────────────────────────
	sort.Strings(subset)
	return subset
}
