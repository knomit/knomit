package synthesize

// The motif axis's calibrate/measurement entry point — Phase-4 Q3's sibling to
// BridgeComponentReport rather than a branch inside it.
//
// WHY A SIBLING. The two reports enumerate different populations over
// different pools with different engines, and the gotcha this project already
// carries about the entity/domain one (calibrate-bridges-floor-heuristic
// 8ad54ee8) is precisely an aggregate whose population was not the production
// population, surviving because nothing forced the population into the output.
// Folding a second population behind a flag on one function is how that
// recurs. Two reports, two stated populations, and both now say theirs in the
// payload.

import (
	"context"
	"fmt"
	"sort"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// ScoredMotifBridge is one enumerated motif bridge candidate — SERVED OR
// DROPPED — with everything the shipped scorer decided about it.
//
// The dropped ones are the point. Production discards them, and every
// measurement the Phase-4 carried-forward register asks for (the near-lane
// crack, §4's unimplemented trim, the vanish rate, the token-2 noise shape) is
// a question about candidates that never reached an agent.
type ScoredMotifBridge struct {
	// Token is the canonical motif id the group is keyed on.
	Token string
	// Members are the member facts' paths, as the enumerator ordered them.
	Members []string
	// Lane is "near" or "far", as laneOf assigned it.
	Lane string
	// Comp holds the raw signal values the scorer computed.
	Comp BridgeComponents
	// Q is the lane's weighted quality score (0 when gated out).
	Q float64
	// Kept reports whether the scorer would serve this candidate, BEFORE the
	// per-lane budget. A kept candidate can still miss a slot — which is the
	// distinction the M3 vanish-rate measurement exists to size.
	Kept bool
	// Cause names why it was dropped, empty when kept. One taxonomy, shared
	// with the health counters, which are tallied from it.
	Cause string
}

// MotifReport is one measurement run over one corpus at one effort.
type MotifReport struct {
	// Population states, in one line, what was enumerated over. Present
	// because a number that travels without its population is how 8ad54ee8
	// happened.
	Population string `json:"population"`
	Branch     string `json:"branch"`
	Effort     string `json:"effort"`
	// Seeds is the size of the pool the enumeration ran over.
	Seeds int `json:"seeds"`
	// SeedsWithMotifs is how many of them carried a motif at all. The axis can
	// only act on these, and the gap between the two numbers is the coverage
	// story restated in the population that matters.
	SeedsWithMotifs int `json:"seeds_with_motifs"`

	Candidates []ScoredMotifBridge `json:"candidates"`

	// NearServed/FarServed are what a session would actually enqueue: kept
	// candidates after suppression, ranking and the per-lane budget.
	NearServed []string `json:"near_served"`
	FarServed  []string `json:"far_served"`
	NearBudget int      `json:"near_budget"`
	FarBudget  int      `json:"far_budget"`

	// The health picture, both as the numbers and as the lines a session emits.
	Ceiling            int      `json:"df_ceiling"`
	OverCeilingNames   []string `json:"over_ceiling_motifs"`
	NearFloorDropped   int      `json:"near_floor_dropped"`
	NearOtherDropped   int      `json:"near_other_dropped"`
	FarOversizeDropped int      `json:"far_oversize_dropped"`
	FarOtherDropped    int      `json:"far_other_dropped"`
	PointCut           int      `json:"disjointness_cut"`
	PointUmbrella      int      `json:"disjointness_umbrella"`
	PointLabels        int      `json:"disjointness_labels"`
	FamilySuppressed   int      `json:"family_suppressed_by_exact"`
	PointStrict        bool     `json:"disjointness_strict"`
	HealthLines        []string `json:"health_lines"`

	// SeedDF2Clusters and SeedBridgeablePairs are the ACTIVATION POPULATION:
	// recurring clusters, and the fact pairs they could bridge, counted over
	// the seed pool rather than over all authored facts.
	//
	// The distinction is the whole point and it is not small. The store's
	// MotifCoverage/VocabularyHealth count AUTHORED facts; the bridging
	// population is the review seed pool, which AcceptSeed filters to
	// Kind == Epistemic. Between a quarter and a half of every measured
	// corpus's motif-bearing facts are pragmatic and can never bridge, so a
	// floor set on the authored counts would be set on a population the axis
	// cannot act on.
	SeedDF2Clusters     int `json:"seed_df2_clusters"`
	SeedBridgeablePairs int `json:"seed_bridgeable_pairs"`

	// Token2Pairs is every pair of canonical ids the token-2 tier would join,
	// with the stems they share — carried-forward register entry 6's "readers
	// of tier numbers must know what produced them".
	//
	// The tier's own predicate computes it (motifStems / sharedMotifStems), so
	// the reported noise cannot drift from the shipped matching. It is a
	// VOCABULARY property, not a candidate count: it says which ids COULD be
	// folded together, before any gate has looked at the facts carrying them.
	Token2Pairs []Token2Pair `json:"token2_pairs"`
}

// Token2Pair is one canonical-id pair the token-2 tier would fold into a
// family, and the stems that did it.
type Token2Pair struct {
	A      string   `json:"a"`
	B      string   `json:"b"`
	Shared []string `json:"shared_stems"`
}

// motifReportPopulation is the one-line population statement.
//
// It says "a FIRST session" deliberately. Production enumerates over the
// review session's seed pool, which is watermark-scoped: an incremental
// session sees only what changed. This report reads the whole corpus, which is
// what a no-watermark session sees and therefore the MAXIMUM the axis can
// enumerate over — an upper bound on candidate counts, not a typical session.
const motifReportPopulation = "live epistemic facts on the branch, projected and filtered by the review " +
	"strategy's own AcceptSeed — i.e. what a FIRST (no-watermark) review session enumerates over. " +
	"An incremental session sees a watermark-scoped subset, so candidate counts here are an upper bound."

// MotifComponentReport enumerates, lane-splits and scores every motif bridge
// candidate on a corpus, and reports the served set beside the dropped one.
//
// It drives the SHIPPED enumeration and scoring — scoreMotifCandidates, the
// same function buildMotifBridges calls — so the numbers Phase 4 sets
// constants on come from the engine that serves the bridges. The seed pool is
// built with production's own projection (factFromSearchResult) and its own
// filter (reviewStrategy.AcceptSeed) for the same reason.
//
// Read-only: it opens nothing, writes nothing, and plans no work.
func MotifComponentReport(
	ctx context.Context,
	idx SearchQuery,
	motifs store.MotifIndex,
	abstraction store.AbstractionIndex,
	branch string,
	eff Effort,
	resolution float64,
	minCommunitySize int,
	cfg QualityConfig,
) (MotifReport, error) {
	rep := MotifReport{
		Population: motifReportPopulation,
		Branch:     branch,
		Effort:     string(eff),
	}

	results, err := idx.Search(ctx, branch, store.SearchOptions{Limit: 100000})
	if err != nil {
		return rep, err
	}
	var strat reviewStrategy
	seeds := make([]factForLLM, 0, len(results))
	for _, r := range results {
		f := factFromSearchResult(r)
		if !strat.AcceptSeed(f) {
			continue
		}
		seeds = append(seeds, factsForLLM([]fact.Fact{f})...)
	}
	rep.Seeds = len(seeds)
	for _, s := range seeds {
		if len(s.Motifs) > 0 {
			rep.SeedsWithMotifs++
		}
	}

	groups, err := ScopedCluster(ctx, seeds, idx, resolution, minCommunitySize, func(ProgressEvent) {}, branch)
	if err != nil {
		return rep, err
	}
	cr := clusterResultFromGroups(groups)

	resolve := motifResolverFor(ctx, motifs, branch)
	labels := subjectLabelsFor(ctx, idx, branch)
	meanSim := meanSimFor(abstraction, branch)

	rows, _, err := scoreMotifCandidates(ctx, idx, branch, seeds, cr, eff, cfg, resolve, labels, meanSim)
	if err != nil {
		return rep, err
	}

	for _, r := range rows {
		paths := make([]string, 0, len(r.cand.Members))
		for _, m := range r.cand.Members {
			paths = append(paths, m.File)
		}
		rep.Candidates = append(rep.Candidates, ScoredMotifBridge{
			Token:   r.cand.Token,
			Members: paths,
			Lane:    string(r.lane),
			Comp:    r.comp,
			Q:       r.q,
			Kept:    r.kept,
			Cause:   string(r.cause),
		})
	}

	// The served set, from the same engine — so the report shows what a session
	// would enqueue, not only what enumeration found.
	// buildMotifBridges' health is the SUPERSET: same enumeration, plus what
	// suppression did — which only exists once the served set is built.
	near, far, health, err := buildMotifBridges(ctx, idx, branch, seeds, cr, eff, cfg, resolve, labels, meanSim)
	if err != nil {
		return rep, err
	}
	for _, b := range near {
		rep.NearServed = append(rep.NearServed, b.Token)
	}
	for _, b := range far {
		rep.FarServed = append(rep.FarServed, b.Token)
	}
	rep.NearBudget, rep.FarBudget = motifSubBudget(eff)

	rep.Ceiling = health.Ceiling
	rep.OverCeilingNames = health.OverCeilingNames
	rep.NearFloorDropped = health.NearFloorDropped
	rep.NearOtherDropped = health.NearOtherDropped
	rep.FarOversizeDropped = health.FarOversizeDropped
	rep.FarOtherDropped = health.FarOtherDropped
	rep.FamilySuppressed = health.FamilySuppressedByExact
	rep.PointCut = health.Point.Cut
	rep.PointUmbrella = health.Point.Umbrella
	rep.PointLabels = health.Point.Labels
	rep.PointStrict = health.Point.Strict
	rep.HealthLines = motifBridgeHealthLines(health, len(near), len(far))
	rep.Token2Pairs = token2PairsOf(seeds, resolve)
	rep.SeedDF2Clusters, rep.SeedBridgeablePairs = seedRecurrence(seeds, resolve)

	return rep, nil
}

// Summary is a one-line human rendering, population first.
func (r MotifReport) Summary() string {
	return fmt.Sprintf(
		"%s @ %s: %d seeds (%d with motifs), %d candidates, %d near / %d far served (budgets %d/%d)",
		r.Branch, r.Effort, r.Seeds, r.SeedsWithMotifs, len(r.Candidates),
		len(r.NearServed), len(r.FarServed), r.NearBudget, r.FarBudget)
}

// token2PairsOf reports every canonical-id pair the token-2 tier would join.
//
// Computed over the ids the seed pool actually carries — the population the
// tier acts on — rather than over the whole alias table, so the number means
// "folds this corpus's bridging population would see".
func token2PairsOf(seeds []factForLLM, resolve motifResolver) []Token2Pair {
	idSet := map[string]struct{}{}
	for _, f := range seeds {
		for _, m := range f.Motifs {
			if c := resolve(m); c != "" {
				idSet[c] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	stems := make([]map[string]struct{}, len(ids))
	for i, id := range ids {
		stems[i] = motifStems(id)
	}
	var out []Token2Pair
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if sh := sharedMotifStems(stems[i], stems[j]); len(sh) >= token2SharedStems {
				out = append(out, Token2Pair{A: ids[i], B: ids[j], Shared: sh})
			}
		}
	}
	return out
}
