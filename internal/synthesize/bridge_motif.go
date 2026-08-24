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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

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

// motifDFCeilingPerCent is the df band's ceiling as a share of the corpus
// (§4: `2 <= df <= max(12, 2%*N)`) — above it a motif has gone generic.
//
// CONSTANT CLASSIFICATION (MN13): a RATIO of the corpus's own size, the same
// class as umbrellaPerCent in the sibling file, and it resolves to a different
// df on every corpus. It was an unnamed inline `* 2 / 100` until the Phase-3
// review (L2) pointed out that the MN13 check bans FLOAT literals and an
// integer-written ratio is invisible to it — lesson 3 turned on the check
// itself: ask in what form the violation would appear, and does the check read
// that form.
const motifDFCeilingPerCent = 2

// motifResolver maps one authored motif spelling to its canonical cluster id.
// An unresolved spelling resolves to ITSELF, so a corpus with no alias table
// behaves as one where every motif is its own singleton cluster.
type motifResolver func(motif string) string

// motifDFFn returns the corpus-wide document frequency of a canonical motif id.
type motifDFFn func(canonicalID string) int

// motifEnumHealth is what enumeration observed. Every field is a DESCRIPTOR:
// nothing in this package branches on any of them.
type motifEnumHealth struct {
	// Failure names why the pass could not run, when it could not. A failed
	// pass that reported "N candidates, 0 near, 0 far" was indistinguishable
	// from an axis that enumerated and found nothing (Phase-3 review, L6) —
	// the same distinction motifResolverFor's warning exists to preserve, one
	// layer up.
	Failure string
	// Candidates is how many groups survived every gate.
	Candidates int
	// Ceiling is the df band's upper bound on this corpus.
	Ceiling int
	// OverCeilingNames are motifs that have gone generic — excluded from
	// bridging and FLAGGED for review splitting (MN8), never silently dropped.
	OverCeilingNames []string
	// Point is the subject-disjointness operating point this corpus derived.
	Point disjointnessPoint
	// NearFloorDropped counts groups assigned to the NEAR lane and then dropped
	// BECAUSE OF THE COHESION FLOOR — the crack between the lanes (Phase-3
	// review, M4).
	//
	// laneOf is binary on `Density > 0`, so a group with one stray SIMILAR_TO
	// edge among otherwise dissimilar members lands in the near lane and is
	// then killed by a 0.5 floor. Such a group is far-lane material by any
	// reading of §4 and disappears with no trace. Visibility now; the lane
	// semantics are redesigned in Phase 4 on this count.
	//
	// It counts ONE cause. The first version incremented on any near-lane
	// rejection, so oversize and quality-floor drops rode in under a label that
	// said "cohesion" — and Phase 4 is going to read this number as evidence
	// for a lane redesign (review M4 counter-finding). A number that does not
	// mean its label is worse than no number.
	NearFloorDropped int
	// NearOtherDropped counts near-lane groups rejected for any OTHER reason —
	// over the member cap, or under the quality floor. Separate because it is
	// not evidence about the lane split.
	NearOtherDropped int
	// FarOversizeDropped counts groups assigned to the FAR lane and then
	// dropped BECAUSE OF THE MEMBER CAP — the population §4's unimplemented
	// trim would act on (carried-forward register entry 5).
	//
	// §4 specifies that an oversized far group is TRIMMED ("maximum community
	// spread, then minimum mean similarity") rather than discarded. The trim
	// was ruled deliberate-to-defer, and the group is dropped by MaxMembers
	// instead — but nothing counted the drops, so the redesign that is supposed
	// to happen on measured data had no denominator at all.
	//
	// IT IS AN UPPER BOUND, twice over, and both matter to whoever reads it as
	// "bridges the trim would recover". A group counted here may also have been
	// failing another gate (the cap is checked first, exactly as the near lane
	// checks its floor first), and a group the trim shrank to fit could still
	// fall under the quality floor afterwards. What the number means is "far
	// groups the member cap rejected" — nothing more.
	FarOversizeDropped int
	// FarOtherDropped counts far-lane groups rejected for any other reason —
	// under the quality floor, or spanning fewer than two communities. Kept
	// apart for the same reason the near lane keeps its two apart: a counter
	// purpose-built for one Phase-4 decision must not quietly carry a second
	// cause under the first one's label (review M4's counter-finding).
	FarOtherDropped int
	// FamilySuppressedByExact counts token-2 families dropped because they
	// strictly contained a verbatim group (Phase-4 rulings-3, amending L1).
	//
	// WHAT READS IT AND WHAT IT MEANS: it is the visible cost of the cross-tier
	// rule. Each one is a looser grouping that did NOT reach an agent, and with
	// it went whatever extra members it had — so a rising count means the token-2
	// tier is mostly re-wrapping groups the exact tier already found, which is
	// evidence about the TIER's value, not about the corpus. It does NOT mean
	// "bridges lost": the exact group each of these contained was served in its
	// place.
	FamilySuppressedByExact int
	// Activation is the per-corpus enablement decision and the counts it was
	// made on. Reported whether or not the axis ran, because "inactive" and
	// "active and found nothing" are different statements about a corpus and a
	// reader comparing sessions must be able to tell them apart (L6, one layer
	// out again).
	Activation motifActivation
}

// enumeratedMotif is an enumerated group plus the TIER that produced it.
//
// The tier is carried rather than re-derived because it cannot be re-derived:
// a token-2 family is keyed by one of the canonical ids it folded, so its
// Token is indistinguishable from a verbatim group's. Suppression has to know
// which is which (see suppressContained), and a wrong guess there drops the
// better group.
type enumeratedMotif struct {
	BridgeSeedSet
	// family reports that this group came from the token-2 tier — a fold of
	// several canonical ids — rather than from one id's own carriers.
	family bool
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
) ([]enumeratedMotif, motifEnumHealth) {
	point := resolveDisjointnessPoint(labels)
	ceiling := labels.LiveFacts * motifDFCeilingPerCent / 100
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
	// The token-2 tier is a UNION, not a replacement (§4: "Union with >= 2
	// shared stemmed motif tokens"). Merging alone would REPLACE each verbatim
	// group with a larger family group, and a larger group is not a superset of
	// the smaller one once the pairwise disjointness pass has run over it: an
	// added member can collide with a member the smaller group kept, and greedy
	// keeps the earlier one. That breaks MN10's "each level's candidates are a
	// strict subset of the next level's" — found on the real knomit-kb corpus,
	// where pairs present at the exact tier vanished at token-2.
	//
	// So the family groups are ADDED to the verbatim ones, and a family whose
	// members are already covered by a verbatim group is dropped as redundant.
	groups := verbatimGroups(byToken)
	if tier == tierToken2 {
		groups = append(groups, token2Families(byToken)...)
	}

	var out []enumeratedMotif
	for _, g := range groups {
		canon, members := g.key, g.members
		// GATE 1 — the df band, `2 <= df <= max(12, 2%*N)` (§4). Below the
		// floor a motif has one carrier and cannot bridge yet. Above the
		// ceiling it has gone generic: excluded, and flagged for splitting.
		d := df(canon)
		// A FAMILY is keyed by one of its ids, whose own df says nothing about
		// how often the family recurs — two hapax spellings of one mechanism
		// are exactly the case this tier exists to catch. Its carriers ARE its
		// recurrence. A verbatim group is NOT promoted this way: there the df
		// is the motif's own, and the floor is what stops a hapax bridging.
		if g.family && len(members) > d {
			d = len(members)
		}
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

		out = append(out, enumeratedMotif{
			BridgeSeedSet: BridgeSeedSet{
				Token:   canon,
				Kind:    BridgeMotif,
				Members: kept,
			},
			family: g.family,
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

// motifGroup is one candidate grouping before the gates run: a key, its
// members, and whether the key names a single canonical id or a token-2 family
// of them.
type motifGroup struct {
	key     string
	members map[string]factForLLM
	family  bool
}

// verbatimGroups projects the exact-tier map into groups, sorted by key so the
// output order never depends on map iteration.
func verbatimGroups(byToken map[string]map[string]factForLLM) []motifGroup {
	keys := make([]string, 0, len(byToken))
	for k := range byToken {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]motifGroup, 0, len(keys))
	for _, k := range keys {
		out = append(out, motifGroup{key: k, members: byToken[k]})
	}
	return out
}

// token2Families returns one group per family of canonical ids sharing >= 2
// stemmed tokens — §4's permissive prefilter, reachable only at high effort.
//
// They are ADDED to the verbatim groups rather than replacing them (§4:
// "Union with >= 2 shared stemmed motif tokens"). Replacement would break MN10:
// a family group is larger, and a larger group is not a superset of the smaller
// one once the pairwise disjointness pass has run over it — an added member can
// collide with one the smaller group kept, and greedy keeps the earlier. That
// was found on the real knomit-kb corpus, where pairs present at the exact tier
// vanished at token-2.
//
// The family's key is the lexicographically smallest id in it, so the choice is
// deterministic and independent of map order. Families are transitive by
// construction: ids are walked in sorted order and each fold moves members onto
// the earliest key, so a chain a~b~c lands entirely on a.
//
// A family that adds no members beyond its verbatim group is dropped: it would
// be the same candidate under a second name, and the agent would judge it twice.
func token2Families(byToken map[string]map[string]factForLLM) []motifGroup {
	ids := make([]string, 0, len(byToken))
	for id := range byToken {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	toks := make([]map[string]struct{}, len(ids))
	for i, id := range ids {
		toks[i] = motifStems(id)
	}

	// families[i] accumulates the members of the family keyed by ids[i].
	families := make([]map[string]factForLLM, len(ids))
	folded := make([]bool, len(ids))
	for i := range ids {
		if folded[i] {
			continue
		}
		for j := i + 1; j < len(ids); j++ {
			if folded[j] {
				continue
			}
			if len(sharedMotifStems(toks[i], toks[j])) < token2SharedStems {
				continue
			}
			if families[i] == nil {
				families[i] = copyMembers(byToken[ids[i]])
			}
			for p, f := range byToken[ids[j]] {
				families[i][p] = f
			}
			folded[j] = true
		}
	}

	var out []motifGroup
	for i, fam := range families {
		if fam == nil || len(fam) <= len(byToken[ids[i]]) {
			continue // adds nothing the verbatim group did not already have
		}
		out = append(out, motifGroup{key: ids[i], members: fam, family: true})
	}
	return out
}

func copyMembers(in map[string]factForLLM) map[string]factForLLM {
	out := make(map[string]factForLLM, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// token2SharedStems is the tier's name: how many stemmed tokens two canonical
// ids must share to join one family.
//
// CONSTANT CLASSIFICATION (MN13, class 2): a STRUCTURAL K with system
// precedent — §4 names the tier "token-2" and the number IS the tier. It
// claims nothing about any corpus's distribution.
const token2SharedStems = 2

// motifStems is the stemmed token set of a canonical motif id — the unit the
// token-2 tier matches on.
//
// One definition, shared by the tier itself and by the measurement that
// reports the tier's noise shape (carried-forward register entry 6). A second
// copy in the instrument would let the measured noise drift from the shipped
// matching, which is the exact class of error the instrument exists to avoid.
func motifStems(id string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range textnorm.Tokens(textnorm.Canonicalize(id)) {
		out[t] = struct{}{}
	}
	return out
}

// sharedMotifStems returns the stems two canonical ids have in common, sorted
// so callers that report them are deterministic.
func sharedMotifStems(a, b map[string]struct{}) []string {
	var out []string
	for t := range a {
		if _, ok := b[t]; ok {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
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
// pairCosFn returns EVERY member-pair body cosine, not just their mean.
//
// The far lane only ever wanted the mean, but §8's novelty signals need the
// distribution: "any pair over dedup" is a question about individual pairs that
// a mean cannot answer — a group with one near-duplicate pair and three distant
// ones has an unremarkable mean and is exactly the case the signal exists to
// surface.
type pairCosFn func(ctx context.Context, paths []string) ([]float64, error)

// dedupThresholdFor reads the dedup cosine OverDedup is measured against.
//
// It is the model-dependent threshold discovery's own duplicate check uses
// (store.EmbedderThresholds), never a number of this file's own — a novelty
// signal calibrated against a different threshold from the gate it describes
// would be measuring nothing anybody acts on.
func dedupThresholdFor(idx SearchQuery) float64 {
	if e, ok := idx.(interface{ Embedder() store.Embedder }); ok {
		return store.EmbedderThresholds(e.Embedder()).Dedup
	}
	return store.EmbedderThresholds(nil).Dedup
}

// noveltyOf computes §8's seed-set novelty signals for one candidate.
//
// EntityJaccard needs no vectors and is always computed. The other two need a
// vector source, so VectorsRead reports whether they mean anything — a zero
// SeedCos from "no vectors" and a zero from "genuinely dissimilar" are opposite
// findings and must not share a representation.
func noveltyOf(ctx context.Context, members []factForLLM, paths []string, pairCos pairCosFn, dedup float64) (NoveltySignals, error) {
	n := NoveltySignals{EntityJaccard: meanEntityJaccard(members)}
	if pairCos == nil {
		return n, nil
	}
	cs, err := pairCos(ctx, paths)
	if err != nil {
		return n, err
	}
	if len(cs) == 0 {
		return n, nil
	}
	n.VectorsRead = true
	var sum float64
	over := 0
	for _, c := range cs {
		sum += c
		if c >= dedup {
			over++
		}
	}
	n.SeedCos = sum / float64(len(cs))
	n.OverDedup = float64(over) / float64(len(cs))
	return n, nil
}

// meanEntityJaccard is the mean Jaccard overlap of member ENTITY sets over all
// member pairs — the subject-axis counterpart to SeedCos.
//
// A pair with no entities on either side contributes 0, not 1: "two facts that
// name nothing" is an absence of evidence about their subjects, and scoring it
// as perfect disjointness would reward the corpus for being unlabelled.
func meanEntityJaccard(members []factForLLM) float64 {
	if len(members) < 2 {
		return 0
	}
	sets := make([]map[string]struct{}, len(members))
	for i, m := range members {
		sets[i] = map[string]struct{}{}
		for _, e := range m.Entities {
			sets[i][strings.ToLower(e)] = struct{}{}
		}
	}
	var sum float64
	pairs := 0
	for i := range sets {
		for j := i + 1; j < len(sets); j++ {
			inter, union := 0, len(sets[j])
			for t := range sets[i] {
				if _, ok := sets[j][t]; ok {
					inter++
				} else {
					union++
				}
			}
			pairs++
			if union > 0 {
				sum += float64(inter) / float64(union)
			}
		}
	}
	if pairs == 0 {
		return 0
	}
	return sum / float64(pairs)
}

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
	pairCos pairCosFn,
) (BridgeComponents, float64, bool, error) {
	paths := make([]string, 0, len(cand.Members))
	for _, m := range cand.Members {
		paths = append(paths, m.File)
	}
	gap, err := derivationGap(ctx, paths, idx)
	if err != nil {
		return BridgeComponents{}, 0, false, err
	}
	comp := BridgeComponents{
		Coh:     cohesion(paths, g),
		Sep:     separation(paths, clusterOf),
		Gap:     gap,
		Spec:    sharedSpec,
		Members: len(paths),
	}
	// §8's novelty signals. Computed for EVERY candidate, on both lanes and
	// whether or not it is kept, because `calibrate bridges` is about the
	// distribution the scorer sees rather than the slice it serves.
	nov, err := noveltyOf(ctx, cand.Members, paths, pairCos, dedupThresholdFor(idx))
	if err != nil {
		return comp, 0, false, err
	}
	comp.Novelty = nov
	if lane == LaneNear {
		q, kept := bridgeQ(comp, cfg)
		return comp, q, kept, nil
	}
	if comp.Sep < 2 || comp.Members > cfg.MaxMembers {
		return comp, 0, false, nil
	}
	ms, err := meanPairCos(ctx, paths, pairCos)
	if err != nil {
		// Propagated, never defaulted. A similarity that could not be read is
		// not "maximally dissimilar", and treating it as 0 would hand every
		// unreadable group the highest far-lane score there is.
		return comp, 0, false, err
	}
	q := cfg.WSpec*comp.Spec + cfg.WGap*comp.Gap + cfg.WCoh*(1-ms)
	return comp, q, q >= cfg.QualityFloor, nil
}

// sharedMotifSpecificity is §4's `Sum(1/df_canonical)` over every canonical
// motif that ALL members carry.
//
// This is where "two shared canonical motifs = rank boost, never a gate" lives:
// the group already exists on the motif it is keyed by, and a second shared one
// only adds a term. Nothing in here can reject a candidate.
//
// A member spelling the same cluster two ways votes once — otherwise a group
// would be strengthened by one author's phrasing habit rather than by a second
// regularity. Terms are summed in sorted order so the float accumulation is
// identical run to run.
func sharedMotifSpecificity(ctx context.Context, idx SearchQuery, branch string, cand BridgeSeedSet, resolve motifResolver) (float64, error) {
	if len(cand.Members) == 0 {
		return 0, nil
	}
	counts := map[string]int{}
	for _, m := range cand.Members {
		seen := map[string]struct{}{}
		for _, raw := range m.Motifs {
			c := resolve(raw)
			if c == "" {
				continue
			}
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			counts[c]++
		}
	}
	shared := make([]string, 0, len(counts))
	for c, n := range counts {
		if n == len(cand.Members) {
			shared = append(shared, c)
		}
	}
	sort.Strings(shared)

	// var, not `:= 0.0`: MN13's check on this file forbids float literals in
	// the code outright, and a blanket rule with no carve-out for "but this one
	// is only a zero" is worth more than the keystroke it costs.
	var total float64
	for _, c := range shared {
		s, err := specificity(ctx, branch, c, string(BridgeMotif), idx)
		if err != nil {
			return 0, err
		}
		total += s
	}
	return total, nil
}

// ── effort binding (§5) ───────────────────────────────────────────────────

// motifTier binds the §4 stage-1 matching tier to the effort dial.
//
// The §5 table's medium and high operating points were calibrated as noise-pass
// rates on the name+def EMBEDDING cascade, and the Q9 ruling DEFERRED that tier
// until real vocabularies exist to calibrate against. What ships here is the
// mechanical half of §4's prefilter, which needs no calibration and no
// constant: exact canonical-id equality, widened at high effort by ">= 2 shared
// stemmed motif tokens".
//
// Monotone by construction (MN10): exact is a subset of exact-union-token-2, so
// raising effort can only add candidates, never change what a bridge means.
//
// RECORDED EXPECTATION (designer, 2026-08-23): until Phase-4 calibration,
// high-effort matching sits at the measured token-2 floor rather than the
// research 68% — the delta is what calibration buys back, not a regression.
func motifTier(e Effort) motifMatchTier {
	if e == EffortHigh {
		return tierToken2
	}
	return tierExact
}

// motifSubBudget returns the per-lane caps for motif bridging.
//
// CONSTANT CLASSIFICATION (MN13, class 2): RESOURCE BUDGETS — agent work-item
// slots. They allocate spend and claim nothing about any corpus's
// distribution.
//
// Per-lane, and ADDITIONAL to the entity/domain budget rather than carved out
// of it (MN8): the three axes have different df distributions, and one shared
// pool would let whichever is densest starve the others. The sum stays inside
// the forward-discover priority band, which TestEffortBudget_StaysBelowPriorityBand
// asserts on the total rather than on any one budget.
//
// normal is the bounded 6ce866f8 amendment: verbatim matches only, at most two
// items, near lane only. On any corpus without motifs it enumerates nothing,
// which is why the EffortNormal contract test still passes vacuously (MN5).
//
// WHAT MN10 DOES AND DOES NOT PROMISE HERE (designer ruling, review M3).
// MN10 governs CANDIDATE sets: each level's enumerated candidates are a strict
// subset of the next level's, which holds and is tested. It does NOT promise
// that a bridge SERVED at medium is served again at high — the budgets below
// are the same kind of cap the entity/domain axis has always had, and at high
// effort many more candidates compete for them. On the measured corpus that is
// 113 token-2 groups for 8 near + 8 far slots against 20 at medium for 4, so a
// medium-served item CAN lose its slot to a better-ranked family. That is
// budget arithmetic, not a monotonicity break. Phase 4 measures how often it
// happens and whether the budgets should grow; exact-first ranking was
// considered and rejected — it would starve token-2 families out entirely.
func motifSubBudget(e Effort) (near, far int) {
	switch e {
	case EffortHigh:
		// "Carved from 48" (§5): a sixth of the entity/domain budget to each
		// lane, so motif bridging adds at most a third again to a session's
		// discover items.
		return effortBudget(EffortHigh) / 6, effortBudget(EffortHigh) / 6
	case EffortMedium:
		return min(4, effortBudget(EffortMedium)/3), 0
	}
	return 2, 0
}

// buildMotifBridges is the production builder: enumerate, split by lane, score
// per lane, rank, and cap each lane at its OWN sub-budget.
//
// Unlike buildScoredBridges it is NOT gated on eff.Discovers(): normal effort
// runs the verbatim tier (§5), bounded at two items. What keeps that honest is
// the motif-free short circuit below, not an effort check.
func buildMotifBridges(
	ctx context.Context,
	idx SearchQuery,
	branch string,
	seeds []factForLLM,
	clusters ClusterResult,
	eff Effort,
	cfg QualityConfig,
	resolve motifResolver,
	labels store.SubjectLabelDF,
	pairCos pairCosFn,
) ([]BridgeSeedSet, []BridgeSeedSet, motifEnumHealth, error) {
	rows, health, err := scoreMotifCandidates(ctx, idx, branch, seeds, clusters, eff, cfg, resolve, labels, pairCos)
	if err != nil {
		return nil, nil, health, err
	}
	var near, far []enumeratedMotif
	for _, r := range rows {
		if !r.kept {
			continue
		}
		cand := r.cand
		cand.Q = r.q
		if r.lane == LaneNear {
			near = append(near, cand)
		} else {
			far = append(far, cand)
		}
	}
	nearBudget, farBudget := motifSubBudget(eff)
	nearOut, nearSup := rankAndCap(near, nearBudget)
	farOut, farSup := rankAndCap(far, farBudget)
	health.FamilySuppressedByExact = nearSup + farSup
	return nearOut, farOut, health, nil
}

// scoredMotifRow is one enumerated candidate and everything the scorer decided
// about it — INCLUDING the ones that were dropped, with the cause.
//
// Production throws the dropped rows away; measurement is mostly about them.
// Both read this one function, so the numbers Phase 4 sets constants on come
// from the engine that serves the bridges rather than from a second
// implementation that agrees with it until it does not.
type scoredMotifRow struct {
	cand  enumeratedMotif
	lane  BridgeLane
	comp  BridgeComponents
	q     float64
	kept  bool
	cause motifDropCause
}

// motifDropCause names why a candidate was not served. Empty when it was.
//
// One taxonomy, and the health counters are TALLIED FROM IT rather than
// incremented alongside it — so a counter and the row it came from cannot
// drift into disagreeing about the same group, which is the shape of the
// review's M4 counter-finding.
type motifDropCause string

const (
	motifKept        motifDropCause = ""
	motifNearFloor   motifDropCause = "near-cohesion-floor"
	motifNearOther   motifDropCause = "near-other"
	motifFarOversize motifDropCause = "far-oversize"
	motifFarOther    motifDropCause = "far-other"
)

// scoreMotifCandidates enumerates, lane-splits and scores every motif bridge
// candidate, returning one row per candidate. It applies no budget and no
// ranking: those belong to the caller that serves items, not to the caller
// that measures them.
func scoreMotifCandidates(
	ctx context.Context,
	idx SearchQuery,
	branch string,
	seeds []factForLLM,
	clusters ClusterResult,
	eff Effort,
	cfg QualityConfig,
	resolve motifResolver,
	labels store.SubjectLabelDF,
	pairCos pairCosFn,
) ([]scoredMotifRow, motifEnumHealth, error) {
	// A corpus with no motifs costs nothing at all — not one index call. This
	// is the mechanism behind MN5's vacuous pass, so it is stated here rather
	// than left to emerge from the gates downstream.
	if !anyMotifs(seeds) {
		return nil, motifEnumHealth{}, nil
	}

	// THE ACTIVATION FLOOR (§8 as amended, phase4-rulings-4). Placed here,
	// beside the motif-free short circuit and before the first index call, for
	// the same reason that one is: an inactive corpus must cost nothing. It is
	// recomputed every session from the pool in hand — level-triggered, no
	// stored flag, nothing to migrate — so a corpus crosses the floor by
	// accumulating recurrence and needs no one to switch it on.
	act := motifActive(seeds, resolve)
	if !act.Active {
		return nil, motifEnumHealth{Activation: act}, nil
	}

	dfOf := func(canon string) int {
		n, err := idx.TokenDF(ctx, branch, canon, string(BridgeMotif))
		if err != nil {
			// An unreadable df cannot bridge: 0 falls below the band's floor,
			// which drops the candidate. Silence would be wrong in the other
			// direction — a huge df would read as "gone generic" and land the
			// motif in the over-ceiling review list it does not belong in.
			return 0
		}
		return n
	}

	cands, health := enumerateMotifCandidates(seeds, clusters, resolve, dfOf, labels, motifTier(eff))
	health.Activation = act
	clusterOf := bridgePathCommunities(seeds, clusters)

	rows := make([]scoredMotifRow, 0, len(cands))
	for _, cand := range cands {
		paths := make([]string, 0, len(cand.Members))
		for _, m := range cand.Members {
			paths = append(paths, m.File)
		}
		g, err := idx.SimilarityAdjacency(ctx, paths)
		if err != nil {
			return nil, health, err
		}
		lane := laneOf(paths, g)

		spec, err := sharedMotifSpecificity(ctx, idx, branch, cand.BridgeSeedSet, resolve)
		if err != nil {
			return nil, health, err
		}
		comp, q, kept, err := scoreMotifCandidate(ctx, cand.BridgeSeedSet, lane, g, idx, branch, clusterOf, cfg, spec, pairCos)
		if err != nil {
			return nil, health, err
		}
		rows = append(rows, scoredMotifRow{
			cand: cand, lane: lane, comp: comp, q: q, kept: kept,
			cause: motifDropCauseOf(lane, comp, cfg, kept),
		})
	}
	tallyMotifDrops(&health, rows)
	return rows, health, nil
}

// motifDropCauseOf names why the scorer rejected a candidate.
//
// Attributed by CAUSE, from the components the scorer computed, rather than by
// "it was near and it went". Each lane names the cause its own Phase-4
// decision turns on FIRST — the near lane's cohesion floor (the crack between
// the lanes), the far lane's member cap (§4's unimplemented trim) — so each is
// an upper bound when a group trips both conditions, as the counters they feed
// say at their definitions.
func motifDropCauseOf(lane BridgeLane, comp BridgeComponents, cfg QualityConfig, kept bool) motifDropCause {
	switch {
	case kept:
		return motifKept
	case lane == LaneNear && comp.Coh < cfg.CohFloor:
		return motifNearFloor
	case lane == LaneNear:
		return motifNearOther
	case comp.Members > cfg.MaxMembers:
		return motifFarOversize
	default:
		return motifFarOther
	}
}

// tallyMotifDrops derives the health counters from the rows, so a counter and
// the row it came from cannot disagree about the same group.
func tallyMotifDrops(h *motifEnumHealth, rows []scoredMotifRow) {
	for _, r := range rows {
		switch r.cause {
		case motifNearFloor:
			h.NearFloorDropped++
		case motifNearOther:
			h.NearOtherDropped++
		case motifFarOversize:
			h.FarOversizeDropped++
		case motifFarOther:
			h.FarOtherDropped++
		}
	}
}

// seedRecurrence counts recurring clusters and the pairs they could bridge,
// over the SEED POOL — the population the activation floor is set on.
//
// A carrier is counted once per canonical id however many spellings of it the
// fact carries, matching what TokenDF and the vocabulary health both do: a
// fact using two spellings of one mechanism is one carrier, not two, or a
// single author's phrasing habit would read as recurrence.
func seedRecurrence(seeds []factForLLM, resolve motifResolver) (df2Clusters, pairs int) {
	carriers := map[string]map[string]struct{}{}
	for _, f := range seeds {
		// The SAME §7 exclusion enumeration applies: a discovered fact is never
		// a seed, or discovery feeds on its own output (cf455b8f). Counting
		// them here would let a corpus activate on recurrence that enumeration
		// cannot see, and the axis would switch on and then find nothing --
		// the activation number would not mean what its label says.
		if f.Origin == string(fact.Discovered) {
			continue
		}
		for _, m := range f.Motifs {
			c := resolve(m)
			if c == "" {
				continue
			}
			if carriers[c] == nil {
				carriers[c] = map[string]struct{}{}
			}
			carriers[c][f.File] = struct{}{}
		}
	}
	for _, set := range carriers {
		n := len(set)
		if n < 2 {
			continue
		}
		df2Clusters++
		pairs += n * (n - 1) / 2
	}
	return df2Clusters, pairs
}

// motifActivationFloor is the minimum number of RECURRING canonical motifs a
// corpus must carry before motif bridging enumerates anything.
//
// CONSTANT CLASSIFICATION (MN13, third class): a STATISTICAL-VALIDITY FLOOR.
// It is a minimum POPULATION, not a proportion and not a claim about any
// corpus's distribution: below it there are too few shared-motif pairs in
// existence for the axis to be doing anything but spending discovery slots on
// noise. Below the floor the mechanism DOES NOTHING, which is what
// distinguishes this class from a threshold someone picked.
//
// WHY THIS QUANTITY. The blueprint's original condition was ~30% motif
// coverage plus non-zero recurrence. Measured, coverage is a BACKFILL RATE
// LIMIT — it tracks sessions elapsed, not corpus character — and the only
// corpus in the gate annex that cleared 30% is the one the annex names as its
// negative result (knomit-io-kb: 100% coverage, and zero recurring clusters
// once the seed filter is applied). The population that can bridge at all is
// what actually limits the axis. Designer ruling, phase4-rulings-4, replacing
// the coverage condition; "non-zero recurrence" is the K=1 degenerate case of
// this same condition and is not a second one.
//
// WHY 3, AND WHAT THAT IS WORTH. Measured post-AcceptSeed df>=2 counts:
// agentic-engineering 4, merged 6, knomit-kb 1, knomit-io-kb 0, core 0. Any K
// in 2..4 separates the two corpora the annex credits with producing value
// from the three it does not; K=1 admits the machinery-only corpus and K=5
// switches off the corpus where the value was measured. 3 sits in the middle
// rather than on either edge. It is a fit through five points with the target
// separation known in advance — recorded as such — redeemed by the fact that
// the QUANTITY has an argument older than the table ("two shots is not a
// sample", gate annex §4).
//
// SETTING IT LOW-INFORMATION IS SAFE BECAUSE ACTIVATION IS LEVEL-TRIGGERED.
// It is recomputed from current counts every session (Q2: computed, never
// configured). A corpus below the floor is not switched off, it is NOT YET ON,
// and it turns itself on by accumulating recurrence — no flag, no migration,
// no user action. So the asymmetry favours caution: too high costs a delay the
// corpus resolves by itself, too low spends slots on a vocabulary that cannot
// support them.
const motifActivationFloor = 3

// motifActivation is what the activation decision saw.
type motifActivation struct {
	// Evaluated reports that the decision was actually MADE. A zero value means
	// the axis was never asked — a motif-free corpus short-circuits before the
	// floor is consulted — and that is a different statement from "asked, and
	// the corpus is below the floor". Without this the two are the same struct,
	// and the health line would announce an inactive corpus for one that simply
	// has no motifs (L6's distinction, one layer further down).
	Evaluated bool
	// Active reports whether the axis enumerates on this corpus this session.
	Active bool
	// DF2Clusters is the recurring-cluster count the decision was made on,
	// over the SEED POOL (see seedRecurrence).
	DF2Clusters int
	// Pairs is the bridgeable-pair ceiling — reported beside the decision
	// because one heavily-shared motif and three df-2 motifs are different
	// situations with the same cluster count.
	Pairs int
}

// motifActive decides whether this corpus has enough recurring vocabulary for
// bridging to mean anything. Pure, and it costs no index call: the resolver is
// already built and the seed pool is already in hand.
func motifActive(seeds []factForLLM, resolve motifResolver) motifActivation {
	clusters, pairs := seedRecurrence(seeds, resolve)
	return motifActivation{
		Evaluated:   true,
		Active:      clusters >= motifActivationFloor,
		DF2Clusters: clusters,
		Pairs:       pairs,
	}
}

// anyMotifs reports whether the seed pool carries a motif at all.
func anyMotifs(seeds []factForLLM) bool {
	for _, f := range seeds {
		if len(f.Motifs) > 0 {
			return true
		}
	}
	return false
}

// rankAndCap orders by Q descending, Token ascending, and truncates to the
// lane's own budget. A zero budget means the lane is closed at this effort.
func rankAndCap(in []enumeratedMotif, budget int) ([]BridgeSeedSet, int) {
	if budget == 0 {
		return nil, 0
	}
	in, crossTier := suppressContained(in)
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Q != in[j].Q {
			return in[i].Q > in[j].Q
		}
		return in[i].Token < in[j].Token
	})
	if len(in) > budget {
		in = in[:budget]
	}
	out := make([]BridgeSeedSet, 0, len(in))
	for _, c := range in {
		out = append(out, c.BridgeSeedSet)
	}
	return out, crossTier
}

// ── review-pipeline wiring ────────────────────────────────────────────────

// enqueueDiscover writes one discover work item. One definition, so the
// entity/domain path and both motif lanes cannot drift apart in how an item is
// shaped, keyed, or prioritised.
func enqueueDiscover(ctx context.Context, d Deps, sess *store.PipelineSession, payload DiscoverWorkPayload, clusterKey string, priority float64) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return wrapf(reviewTool, err, "marshal discover payload %s", clusterKey)
	}
	if err := d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "discover",
		ClusterKey: clusterKey,
		FactsJSON:  string(payloadJSON),
		Priority:   priority,
	}); err != nil {
		return wrapf(reviewTool, err, "insert discover item %s", clusterKey)
	}
	return nil
}

// motifResolverFor closes over this session's alias table.
//
// Read ONCE: the resolver is called per motif per fact, and a per-call query
// would turn enumeration into a round-trip storm over a table that does not
// change mid-session.
func motifResolverFor(ctx context.Context, motifs store.MotifIndex, branch string) motifResolver {
	table, err := motifs.AliasTable(ctx, branch)
	if err != nil {
		// Degrade, but never silently: without the table every spelling is its
		// own cluster, so synonyms stop bridging and the session's motif output
		// is quietly narrower than it should be. A reader comparing two
		// sessions needs to know which one ran blind.
		log.Warn().Err(err).Str("branch", branch).
			Msg("review: motif alias table unreadable; bridging on verbatim spellings only")
		table = nil
	}
	return func(m string) string {
		if c, ok := table[m]; ok && c != "" {
			return c
		}
		// An unresolved spelling resolves to ITSELF, so a corpus with no alias
		// table behaves as one where every motif is its own singleton cluster —
		// which is exactly what the verbatim tier wants at normal effort.
		return m
	}
}

// subjectLabelsFor reads the corpus's own label distribution for the
// disjointness gate. On failure it returns the zero value, whose operating
// point is STRICT — the conservative direction, and the one that keeps a
// near-duplicate out rather than letting it through.
func subjectLabelsFor(ctx context.Context, search SearchQuery, branch string) store.SubjectLabelDF {
	labels, err := search.SubjectLabelDF(ctx, branch)
	if err != nil {
		log.Warn().Err(err).Str("branch", branch).
			Msg("review: subject-label distribution unreadable; disjointness gate falls back to strict")
		return store.SubjectLabelDF{}
	}
	return labels
}

// meanSimFor reads REAL cosines from the stored blended vectors.
//
// The far lane cannot get them from the SIMILAR_TO graph — that graph is empty
// there by definition — so this reaches for the vectors the graph was built
// from.
func pairCosFor(abstraction store.AbstractionIndex, branch string) pairCosFn {
	return func(ctx context.Context, paths []string) ([]float64, error) {
		ids, err := abstraction.FactIDsByPath(ctx, branch, paths)
		if err != nil {
			return nil, err
		}
		idList := make([]int64, 0, len(ids))
		for _, id := range ids {
			idList = append(idList, id)
		}
		vecs, err := abstraction.BodyVectorsByFactID(ctx, idList)
		if err != nil {
			return nil, err
		}
		var out []float64
		for i := 0; i < len(idList); i++ {
			for j := i + 1; j < len(idList); j++ {
				a, b := vecs[idList[i]], vecs[idList[j]]
				if len(a) == 0 || len(b) == 0 {
					continue
				}
				out = append(out, dot(a, b))
			}
		}
		return out, nil
	}
}

// meanPairCos is the far lane's view of the pair distribution.
//
// AN EMPTY DISTRIBUTION SCORES 1, NOT 0. No vectors is not "maximally
// dissimilar": returning 0 would hand every un-embedded group the highest
// far-lane score there is, which is the opposite of what missing evidence
// should buy. That rule lived inside the old mean-only provider and is kept
// here rather than lost in the refactor.
func meanPairCos(ctx context.Context, paths []string, pairCos pairCosFn) (float64, error) {
	if pairCos == nil {
		return 1, nil
	}
	cs, err := pairCos(ctx, paths)
	if err != nil {
		return 0, err
	}
	if len(cs) == 0 {
		return 1, nil
	}
	var sum float64
	for _, c := range cs {
		sum += c
	}
	return sum / float64(len(cs)), nil
}

// motifBridgeHealthLines are DESCRIPTORS: no branch in this package reads any
// of them. The operating point is printed precisely because it is a
// fingerprint — the same code on two corpora prints two different df cuts,
// which is what it means for the gate to be a percentile of each repo's own
// distribution rather than a threshold someone picked (MN13).
func motifBridgeHealthLines(h motifEnumHealth, near, far int) []string {
	if h.Failure != "" {
		return []string{fmt.Sprintf(
			"motif bridges unavailable this session: %s "+
				"(no candidates — this is NOT a statement about the corpus)", h.Failure)}
	}
	if h.Activation.Evaluated && !h.Activation.Active {
		return []string{fmt.Sprintf(
			"motif bridging inactive: %d recurring motif(s) (%d bridgeable pair(s)), below the "+
				"%d-motif validity floor — the corpus has too little repeated vocabulary for a "+
				"shared-motif pair to mean anything yet. Recomputed every session; it activates "+
				"itself as recurrence accumulates.",
			h.Activation.DF2Clusters, h.Activation.Pairs, motifActivationFloor)}
	}
	if h.Candidates == 0 && len(h.OverCeilingNames) == 0 {
		return nil
	}
	point := fmt.Sprintf("df <= %d (p%d of %d labels)", h.Point.Cut, disjointnessPercentile, h.Point.Labels)
	if h.Point.Strict {
		// The two fallbacks are DIFFERENT diagnoses and the line says which.
		// "Too few labels to estimate from" and "the estimate came back
		// unsatisfiable" describe different corpora and want different
		// responses; one wording for both would send a reader looking at the
		// wrong thing.
		switch h.Point.Fallback {
		case degenerateCut:
			point = fmt.Sprintf(
				"STRICT fallback (degenerate cut) — %d labels is past the floor, but p%d of their "+
					"df distribution is %d, and a SHARED label has df >= %d by definition, so that "+
					"cut can never block. Any shared label blocks instead",
				h.Point.Labels, disjointnessPercentile, h.Point.Cut, minUsableCut)
		default:
			point = fmt.Sprintf(
				"STRICT fallback (label floor) — %d labels is below the %d-label validity floor, "+
					"so any shared label blocks", h.Point.Labels, minLabelsForPercentile)
		}
	}
	lines := []string{
		fmt.Sprintf("motif bridges: %d candidates, %d near, %d far (df band 2..%d)",
			h.Candidates, near, far, h.Ceiling),
		fmt.Sprintf("motif disjointness: %s; umbrella df > %d", point, h.Point.Umbrella),
	}
	if h.NearFloorDropped > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif near-lane floor: %d group(s) assigned near and dropped below the "+
				"cohesion floor — sparse-similarity groups the lane split does not yet "+
				"route to the far lane", h.NearFloorDropped))
	}
	if h.NearOtherDropped > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif near-lane other: %d group(s) dropped over the member cap or under "+
				"the quality floor — not evidence about the lane split", h.NearOtherDropped))
	}
	if h.FarOversizeDropped > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif far-lane oversize: %d group(s) assigned far and dropped over the "+
				"member cap — the population §4's unimplemented trim would act on "+
				"(upper bound: a trimmed group could still fail another gate)",
			h.FarOversizeDropped))
	}
	if h.FarOtherDropped > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif far-lane other: %d group(s) dropped under the quality floor or "+
				"spanning one community — not evidence about the trim",
			h.FarOtherDropped))
	}
	if h.FamilySuppressedByExact > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif tier suppression: %d token-2 family/families dropped in favour of the "+
				"exact group they contained — the looser grouping did not reach an agent, "+
				"and neither did its extra members", h.FamilySuppressedByExact))
	}
	if len(h.OverCeilingNames) > 0 {
		lines = append(lines, fmt.Sprintf(
			"motif df ceiling: %d motif(s) over the band, flagged for review splitting rather than bridged: %s",
			len(h.OverCeilingNames), strings.Join(h.OverCeilingNames, ", ")))
	}
	return lines
}

// suppressContained resolves groups whose members are a strict subset of
// another group's, and the rule DIFFERS depending on whether the two came from
// the same tier. It returns the survivors and how many cross-tier families it
// dropped.
//
// WITHIN A TIER, the superset survives. The token-2 tier makes these by
// construction: a family is kept only when it has more members than its key's
// verbatim group, so that group is always strictly contained in it. On the
// measured fixture that was 12 of 113 groups (Phase-3 review, L1). Spending two
// of eight slots on {A,B} and {A,B,C} is spending two on one question, and the
// superset carries strictly more evidence for the SAME shared mechanism.
//
// ACROSS TIERS, THE EXACT GROUP WINS, and the family is dropped (designer
// ruling, Phase-4 rulings-3, amending L1). The within-tier rationale does not
// transfer: a token-2 family is a DIFFERENT, LOOSER grouping, not more evidence
// for the same one, so "the superset carries strictly more evidence" is false
// of it. Measured instance that produced the ruling — on the merged corpus at
// high effort, the family keyed `invents-rather-than-asks` folded the genuine
// verbatim pair `own-rather-than-rent` (owning vs renting compute) together
// with a tool-parameter gotcha and a drug-discovery fact, joined by nothing but
// the English construction "rather than". Under L1 as written the family
// displaced the real pair and took the slot.
//
// WHAT THIS COSTS, said plainly: a dropped family's EXTRA members leave the
// served set with it, so pairs only that family offered are no longer offered.
// That is the intended trade — those pairs exist only because a looser tier
// invented the grouping — but it is a real loss, not a free win, and the
// acceptance assertion about it is tier-aware for that reason.
//
// Runs before ranking and truncation, or it would free no slots.
func suppressContained(in []enumeratedMotif) ([]enumeratedMotif, int) {
	sets := make([]map[string]struct{}, len(in))
	for i, g := range in {
		sets[i] = make(map[string]struct{}, len(g.Members))
		for _, m := range g.Members {
			sets[i][m.File] = struct{}{}
		}
	}
	out := make([]enumeratedMotif, 0, len(in))
	crossTier := 0
	for i := range in {
		drop := false
		for j := range in {
			if i == j {
				continue
			}
			// Cross-tier: a FAMILY containing an exact group loses to it,
			// whatever their sizes.
			if in[i].family && !in[j].family && subsetOf(sets[j], sets[i]) {
				drop = true
				crossTier++
				break
			}
			// Same tier: the strict superset survives.
			if in[i].family == in[j].family &&
				len(sets[j]) > len(sets[i]) && subsetOf(sets[i], sets[j]) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, in[i])
		}
	}
	return out, crossTier
}

func subsetOf(small, big map[string]struct{}) bool {
	for k := range small {
		if _, ok := big[k]; !ok {
			return false
		}
	}
	return true
}
