package synthesize

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"knomit/internal/store"
)

// Cousin-meeting inside prune (knomit#149).
//
// THE BLINDER. ScopedCluster expands each seed's neighbourhood with
// `Path: categoryDir(seed.File)` (cluster.go), and that filter is a raw path
// PREFIX — `AND f.path LIKE cat||'%'`. So:
//
//   - SIBLINGS (same directory) meet.
//   - DESCENDANTS of a shallow-filed seed meet, which is why prune clusters
//     look cross-category from outside: a fact filed at a shallow path acts as
//     a hub joining the deep directories under it.
//   - COUSINS — two facts in different directories under a shared ancestor,
//     with no shallow-filed seed bridging them — are unreachable AT ANY COSINE.
//
// Measured on the live core corpus: six confirmed duplicate pairs at blended
// cosine 0.83–0.97, each split across two freeform categories, none
// co-clustered. `network-security/check-point-…` and
// `network-appliances/23d98f38` surfaced in prune items repeatedly and never
// once together. Six independent confirmations across four drain sessions.
//
// WHY THIS AUGMENTS RATHER THAN REPLACES. Removing the fence re-clusters the
// whole corpus and moves what EffortNormal produces. It is also unnecessary:
// the fence is on the neighbour EXPANSION only — `SubgraphEdges` (Step 2) reads
// SIMILAR_TO unfenced — so a second, unfenced expansion whose result reaches
// PRUNE ONLY buys the cousins without touching what anything else clusters.
//
// WHAT THIS DELIBERATELY DOES NOT TOUCH. `clusters` has FIVE consumers in
// reviewStrategy.Plan: dedupCluster's mechanical merge, the prune items,
// planRestatementShortlist's co-membership, distillGroups, and
// clusterResultFromGroups (which feeds both bridge axes and discover). The
// ruling scopes this to prune, so this function takes and returns a SEPARATE
// prune-cluster slice and never mutates the one those other four read.
//
// AND IT RUNS AFTER dedupCluster, not before. Before it, a newly-met cousin
// pair above the floor would be merged MECHANICALLY, with no judge — a strictly
// larger change than the one ruled. The ruling is that cousins should MEET so
// the prune JUDGE sees them.

// maxCousinSearches is a RESOURCE BUDGET: how many unfenced neighbour searches
// one session will spend looking for cousins.
//
// MN13 classification — it bounds WORK, and encodes no claim about how many
// cousins a corpus has. Each search is one bounded KNN over a stored vector
// (QueryByPath re-embeds nothing), the same unit ScopedCluster and dedupCluster
// already spend per seed; this caps how many of them the cousin pass adds on
// top. When the cap truncates the sweep the health line SAYS SO — a silent cap
// reads as "covered everything" and is the failure this area keeps hitting.
const maxCousinSearches = 64

// maxConcurrentCousinSearches bounds in-flight searches, matching
// maxConcurrentDedupSearches and maxConcurrentNeighborSearches. Same rationale:
// each search is I/O-bound and independent.
const maxConcurrentCousinSearches = 8

// cousinHealth is what the pass reports about itself. Observability only — no
// branch reads it.
type cousinHealth struct {
	Searched  int  // facts whose unfenced neighbourhood was actually read
	Candidate int  // facts in prune clusters, i.e. what a complete sweep would be
	Attached  int  // cousins pulled into an existing prune cluster
	Joined    int  // pairs of prune clusters merged because a cousin linked them
	Truncated bool // the budget cut the sweep short
	Failure   string
}

// joinCousinsForPrune widens the PRUNE cluster set — and only that set — so
// that facts the category fence kept apart are judged together.
//
// The search is deliberately the same one dedupCluster runs (QueryByPath, the
// model's own dedup floor, no path filter) with the membership rule INVERTED:
// dedupCluster keeps only hits already inside the cluster, and this keeps only
// hits outside it. That symmetry is the point — the two halves of one
// neighbourhood, one already handled and one previously discarded.
//
// Degrades to "no cousins" rather than failing the session: this is an addition
// to consolidation, and a corpus whose search backend is unhappy should still
// get its ordinary review. A failure becomes a health line, never silence.
func joinCousinsForPrune(
	ctx context.Context,
	d Deps,
	branch string,
	pruneClusters [][]factForLLM,
	threshold float64,
) ([][]factForLLM, cousinHealth) {
	var h cousinHealth
	if len(pruneClusters) == 0 {
		return pruneClusters, h
	}

	// Which prune cluster each fact currently sits in. A fact in no prune
	// cluster is absent, which is exactly the "leftover" case the measured
	// pairs took: one twin in a prune item, the other in the rest bucket.
	clusterOf := make(map[string]int)
	for i, c := range pruneClusters {
		for _, f := range c {
			clusterOf[f.File] = i
		}
	}

	// The sweep order is fixed BY PATH before the budget is applied, so which
	// facts get searched is a function of the SET of prune facts and nothing
	// else — not of the order Louvain happened to emit its communities in.
	//
	// Collecting from a slice is already ordered, so this sort is not what makes
	// a single run reproducible; it is what makes the truncated tail independent
	// of CLUSTER ORDER. Without it, the same corpus clustered into the same
	// groups in a different sequence sweeps a different subset and produces
	// different prune items — a wobble of exactly the kind cluster.go's own
	// sort.Strings exists to stop, one layer down.
	var targets []string
	for _, c := range pruneClusters {
		for _, f := range c {
			targets = append(targets, f.File)
		}
	}
	sort.Strings(targets)
	h.Candidate = len(targets)
	if len(targets) > maxCousinSearches {
		targets = targets[:maxCousinSearches]
		h.Truncated = true
	}
	h.Searched = len(targets)

	// A cousin edge: a fact in a prune cluster, and a neighbour outside it.
	type cousinEdge struct {
		from string // path in a prune cluster
		hit  factForLLM
	}
	var edges []cousinEdge
	var mu sync.Mutex
	var searchErr error

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxConcurrentCousinSearches)
	for _, path := range targets {
		g.Go(func() error {
			// NO `Path` filter. That single omission IS the fix — everything
			// else here is bookkeeping around it.
			results, err := d.Search.Search(gctx, branch, store.SearchOptions{
				QueryByPath:   path,
				MinSimilarity: threshold,
				Limit:         10,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// Record the first failure and carry on: one unhappy search
				// must not cost the session its other cousins.
				if searchErr == nil {
					searchErr = err
				}
				return nil
			}
			for _, r := range results {
				if r.Path == path {
					continue // self-match
				}
				if other, ok := clusterOf[r.Path]; ok && other == clusterOf[path] {
					continue // siblings: they already meet
				}
				// Kind is NOT filtered here, matching ScopedCluster's own
				// neighbour expansion — a neighbour it pulls in is not
				// kind-checked either. Introducing a filter on this path alone
				// would make the two halves of one neighbourhood disagree.
				edges = append(edges, cousinEdge{from: path, hit: factForLLM{
					File: r.Path, Title: r.Title, Body: r.Body,
					Type: r.Type, Domain: r.Domain, Entities: r.Entities,
					Motifs: r.Motifs, Confidence: r.Confidence, Sources: r.Sources,
				}})
			}
			return nil
		})
	}
	// The goroutines swallow their own errors, so Wait is a barrier.
	_ = g.Wait()

	if searchErr != nil {
		h.Failure = "cousin search failed"
		log.Warn().Err(searchErr).Msg("review: cousin search failed; prune clusters keep their category-scoped membership")
	}
	if len(edges) == 0 {
		return pruneClusters, h
	}

	// Deterministic application order, for the same reason cluster.go sorts its
	// subgraph paths: the searches ran concurrently, so `edges` arrives in
	// completion order. Union-find over an unsorted edge list can produce
	// different cluster groupings for identical input.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		return edges[i].hit.File < edges[j].hit.File
	})

	// Union-find over prune-cluster indices. Two clusters merge when a cousin
	// edge links them; a cousin in NO cluster is attached to the one that found
	// it.
	parent := make([]int, len(pruneClusters))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}

	attached := make(map[int][]factForLLM)
	attachedSeen := map[string]bool{}
	for _, e := range edges {
		src := find(clusterOf[e.from])
		if dstIdx, inCluster := clusterOf[e.hit.File]; inCluster {
			dst := find(dstIdx)
			if src != dst {
				parent[dst] = src
				h.Joined++
			}
			continue
		}
		if attachedSeen[e.hit.File] {
			continue
		}
		attachedSeen[e.hit.File] = true
		attached[src] = append(attached[src], e.hit)
		h.Attached++
	}

	// Rebuild. Iterating by index keeps the output order a function of the
	// input order, so cluster keys stay stable for an unchanged corpus.
	merged := make(map[int][]factForLLM)
	var roots []int
	for i, c := range pruneClusters {
		root := find(i)
		if _, seen := merged[root]; !seen {
			roots = append(roots, root)
		}
		merged[root] = append(merged[root], c...)
	}
	for root, extra := range attached {
		merged[find(root)] = append(merged[find(root)], extra...)
	}

	// NO de-duplication pass here, and that is a conclusion rather than an
	// omission. A first draft ran the output through a dedupeByPath helper "in
	// case a cousin is reached from two members of one cluster". Sabotage S16
	// deleted it and every test stayed green, which sent me to check whether it
	// could ever fire: it cannot. `attachedSeen` already admits each cousin
	// exactly once across the whole pass, and the attach branch is reached only
	// when `clusterOf` has no entry for the hit — so an attached fact is by
	// construction in no cluster, and the union branch only ever re-parents
	// disjoint Louvain communities. The only input that could produce a
	// duplicate is one whose clusters already overlap, which is a malformed
	// cluster set rather than something this function should paper over.
	// An unreachable guard is worse than no guard: it invites a test that
	// asserts nothing (fixture vacuity) and it implies a hazard that does not
	// exist.
	out := make([][]factForLLM, 0, len(roots))
	for _, root := range roots {
		out = append(out, merged[root])
	}
	return out, h
}

// cousinSignalLine reports what the cousin pass did.
//
// It reports even when it found nothing, on this area's standing rule: absence
// of work must be STATED, never inferred. A pass that is silent when it finds
// nothing is indistinguishable from a pass that did not run — which is exactly
// how the category fence stayed invisible for as long as it did.
func cousinSignalLine(h cousinHealth) string {
	if h.Failure != "" {
		return fmt.Sprintf("cross-category prune: %s; prune clusters keep their "+
			"category-scoped membership this session", h.Failure)
	}
	line := fmt.Sprintf(
		"cross-category prune: %d of %d prune facts swept for cousins, "+
			"%d cousin(s) attached, %d cluster pair(s) joined",
		h.Searched, h.Candidate, h.Attached, h.Joined)
	if h.Truncated {
		line += fmt.Sprintf(". SWEEP TRUNCATED at the %d-search budget — %d prune "+
			"facts were not swept this session, so absence of cousins is NOT "+
			"established for them", maxCousinSearches, h.Candidate-h.Searched)
	}
	return line
}
