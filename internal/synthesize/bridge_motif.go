package synthesize

// Motif bridging — blueprint §4/§5. The aspect axis: facts about unrelated
// subjects grouped by the MECHANISM they both instantiate.
//
// MN2 (the cf455b8f invariant): nothing in this file calls an LLM, and
// enumeration calls no index either. The canonical-id resolver, the df reader
// and the label distribution are injected by the caller, which keeps the
// detector mechanical by construction and lets every gate be tested without a
// database.

import (
	"context"
	"sort"

	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/textnorm"
)

// motifMatchTier is the §4 stage-1 matching tier, bound to the effort dial.
type motifMatchTier int

const (
	// tierExact is canonical-id equality — the verbatim tier, and the highest
	// precision event measured in every experiment (§12-E3).
	tierExact motifMatchTier = iota
	// tierToken2 additionally groups canonical ids sharing >= 2 stemmed tokens:
	// the mechanical half of §4's permissive prefilter.
	tierToken2
)

// motifDFCeilingFloor keeps the df band's ceiling meaningful on small corpora.
//
// CONSTANT CLASSIFICATION (MN13, third class): a STATISTICAL-VALIDITY FLOOR on
// a DERIVED value. The band's ceiling is 2% of the corpus (§4), and 2% of 200
// facts is 4 — which would call any normally-recurring motif "gone generic"
// and bridge nothing. Below the corpus size where 2% is a usable estimate, the
// band uses this instead. It claims nothing about any corpus's distribution;
// it says where the estimate stops being one.
const motifDFCeilingFloor = 12

// motifResolver maps one authored motif spelling to its canonical cluster id.
// An unresolved spelling resolves to ITSELF, so a corpus with no alias table
// behaves as one where every motif is its own singleton cluster.
type motifResolver func(motif string) string

// motifDFFn returns the corpus-wide document frequency of a canonical motif id.
type motifDFFn func(canonicalID string) int

// motifEnumHealth is what enumeration observed. Every field is a DESCRIPTOR:
// nothing in this package branches on any of them.
type motifEnumHealth struct {
	// Candidates is how many groups survived every gate.
	Candidates int
	// Ceiling is the df band's upper bound on this corpus.
	Ceiling int
	// OverCeilingNames are motifs that have gone generic — excluded from
	// bridging and FLAGGED for review splitting (MN8), never silently dropped.
	OverCeilingNames []string
	// Point is the subject-disjointness operating point this corpus derived.
	Point disjointnessPoint
}

// enumerateMotifCandidates is the §4 enumeration loop, with the gates applied
// in §4 order: df band, then Louvain separation, then subject-disjointness.
//
// It returns groups keyed on the CANONICAL motif id, path-sorted members,
// token-sorted output — deterministic in full, so map iteration order can never
// reach a work item.
func enumerateMotifCandidates(
	seeds []factForLLM,
	clusters ClusterResult,
	resolve motifResolver,
	df motifDFFn,
	labels store.SubjectLabelDF,
	tier motifMatchTier,
) ([]BridgeSeedSet, motifEnumHealth) {
	point := resolveDisjointnessPoint(labels)
	ceiling := labels.LiveFacts * 2 / 100
	if ceiling < motifDFCeilingFloor {
		ceiling = motifDFCeilingFloor
	}
	health := motifEnumHealth{Ceiling: ceiling, Point: point}

	pathCom := bridgePathCommunities(seeds, clusters)

	// canonical id -> path -> fact.
	byToken := map[string]map[string]factForLLM{}
	for _, f := range seeds {
		// §7 idempotency: a discovered fact is never a seed, or discovery feeds
		// on its own output (cf455b8f).
		if f.Origin == string(fact.Discovered) {
			continue
		}
		for _, m := range f.Motifs {
			c := resolve(m)
			if c == "" {
				continue
			}
			if byToken[c] == nil {
				byToken[c] = map[string]factForLLM{}
			}
			byToken[c][f.File] = f
		}
	}
	if tier == tierToken2 {
		mergeToken2Groups(byToken)
	}

	var out []BridgeSeedSet
	for canon, members := range byToken {
		// GATE 1 — the df band, `2 <= df <= max(12, 2%*N)` (§4). Below the
		// floor a motif has one carrier and cannot bridge yet. Above the
		// ceiling it has gone generic: excluded, and flagged for splitting.
		d := df(canon)
		if d > ceiling {
			health.OverCeilingNames = append(health.OverCeilingNames, canon)
			continue
		}
		if d < 2 {
			continue
		}

		// GATE 3 — subject-disjointness, applied pairwise (see disjointMembers).
		// Ordered after the df band because it is the expensive one, and before
		// separation because dropping a member can change the community span.
		kept := disjointMembers(members, labels, point)
		if len(kept) < 2 {
			continue
		}

		// GATE 2 — Louvain separation >= 2. A GATE, never a ranking reward
		// (1f536807): members in one community are neighbours, not a bridge.
		coms := map[int]struct{}{}
		for _, m := range kept {
			coms[pathCom[m.File]] = struct{}{}
		}
		if len(coms) < 2 {
			continue
		}

		out = append(out, BridgeSeedSet{
			Token:   canon,
			Kind:    BridgeMotif,
			Members: kept,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	sort.Strings(health.OverCeilingNames)
	health.Candidates = len(out)
	return out, health
}

// disjointMembers applies the subject-disjointness gate pairwise, keeping a
// member only when it is disjoint from every member already kept.
//
// Greedy rather than all-or-nothing: one member sharing a subject with one
// other must not delete a group of four that is otherwise a genuine bridge.
// Members are path-sorted first, so which member survives a collision is
// deterministic rather than a function of map iteration order.
func disjointMembers(members map[string]factForLLM, labels store.SubjectLabelDF, p disjointnessPoint) []factForLLM {
	all := make([]factForLLM, 0, len(members))
	for _, f := range members {
		all = append(all, f)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].File < all[j].File })

	var kept []factForLLM
	for _, cand := range all {
		ok := true
		for _, k := range kept {
			if !subjectDisjoint(cand, k, labels, p) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, cand)
		}
	}
	return kept
}

// mergeToken2Groups folds canonical ids sharing >= 2 stemmed tokens into one
// group — the mechanical half of §4's permissive prefilter, reachable only at
// high effort.
//
// The surviving key is the lexicographically smallest id of a merged set, so
// the choice is deterministic and independent of map order. Merging is
// transitive by construction: ids are processed in sorted order and each fold
// moves members into the earlier key, so a chain a~b~c lands entirely on a.
func mergeToken2Groups(byToken map[string]map[string]factForLLM) {
	ids := make([]string, 0, len(byToken))
	for id := range byToken {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	toks := make([]map[string]struct{}, len(ids))
	for i, id := range ids {
		toks[i] = map[string]struct{}{}
		for _, t := range textnorm.Tokens(textnorm.Canonicalize(id)) {
			toks[i][t] = struct{}{}
		}
	}
	for i := range ids {
		if byToken[ids[i]] == nil {
			continue // already folded into an earlier id
		}
		for j := i + 1; j < len(ids); j++ {
			if byToken[ids[j]] == nil {
				continue
			}
			shared := 0
			for t := range toks[i] {
				if _, ok := toks[j][t]; ok {
					shared++
				}
			}
			if shared < 2 {
				continue
			}
			for p, f := range byToken[ids[j]] {
				byToken[ids[i]][p] = f
			}
			delete(byToken, ids[j])
		}
	}
}

// ── the lane split (§4) ───────────────────────────────────────────────────

// BridgeLane is the §4 lane split. Members that are SIMILAR_TO neighbours form
// the NEAR lane; members with no edge between them form the FAR lane, where
// cohesion is 0 by construction.
//
// The two lanes mean different things and route differently: near is "similar
// AND sharing a named mechanism", which plausibly supports an entailed
// consequence and routes forward; far is the novel-analogy class this design
// exists for, and routes backward as a hypothesis under default-NO.
type BridgeLane string

const (
	LaneNear BridgeLane = "near"
	LaneFar  BridgeLane = "far"
)

// laneOf assigns a candidate to its lane.
func laneOf(paths []string, g store.SimilarityGraph) BridgeLane {
	if g.Density(paths) > 0 {
		return LaneNear
	}
	return LaneFar
}

// meanSimFn returns the mean pairwise similarity of the members.
//
// It is injected because the far lane needs REAL cosines and the SIMILAR_TO
// graph cannot supply them: that graph is a top-K edge set, and in the far lane
// it is empty by definition, so a "similarity" read off it would be the same
// constant for every far candidate — a term that ranks nothing.
type meanSimFn func(ctx context.Context, paths []string) (float64, error)

// scoreMotifCandidate scores one candidate on its lane.
//
// Near lane: the existing bridgeQ, unchanged — cohesion floor, separation >= 2,
// size cap. A near-lane group below the cohesion floor is DROPPED, not
// re-routed to the far lane: the lanes partition the candidates, they are not a
// retry.
//
// Far lane: Q = WSpec*sharedSpec + WGap*Gap + WCoh*(1 - meanSim). The cohesion
// FLOOR is not applied — cohesion is 0 there by construction and the floor
// would reject every far candidate — but separation and the size cap still are.
// The dissimilarity term takes cohesion's weight rather than adding a knob, so
// both lanes' scores stay on one scale and neither needs its own calibration.
//
// reshapeCohesiveSubset is NEVER used on this path (§4): it is cohesion-driven
// and would reassemble the near subset out of a far group. Oversized far groups
// are dropped by the size cap; the §4 trim is carried forward to Phase 4.
func scoreMotifCandidate(
	ctx context.Context,
	cand BridgeSeedSet,
	lane BridgeLane,
	g store.SimilarityGraph,
	idx SearchQuery,
	branch string,
	clusterOf map[string]int,
	cfg QualityConfig,
	sharedSpec float64,
	meanSim meanSimFn,
) (float64, bool, error) {
	paths := make([]string, 0, len(cand.Members))
	for _, m := range cand.Members {
		paths = append(paths, m.File)
	}
	gap, err := derivationGap(ctx, paths, idx)
	if err != nil {
		return 0, false, err
	}
	comp := BridgeComponents{
		Coh:     cohesion(paths, g),
		Sep:     separation(paths, clusterOf),
		Gap:     gap,
		Spec:    sharedSpec,
		Members: len(paths),
	}
	if lane == LaneNear {
		q, kept := bridgeQ(comp, cfg)
		return q, kept, nil
	}
	if comp.Sep < 2 || comp.Members > cfg.MaxMembers {
		return 0, false, nil
	}
	ms, err := meanSim(ctx, paths)
	if err != nil {
		// Propagated, never defaulted. A similarity that could not be read is
		// not "maximally dissimilar", and treating it as 0 would hand every
		// unreadable group the highest far-lane score there is.
		return 0, false, err
	}
	q := cfg.WSpec*comp.Spec + cfg.WGap*comp.Gap + cfg.WCoh*(1-ms)
	return q, q >= cfg.QualityFloor, nil
}
