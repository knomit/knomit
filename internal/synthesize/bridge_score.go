package synthesize

import (
	"context"

	"knomit/internal/store"
)

// BridgeComponents holds the raw signal values computed for a bridge seed set.
// All fields are exported so the calibrate tool (a later Phase-3 task) can
// inspect them across the package boundary.
type BridgeComponents struct {
	Coh     float64 // intra-cluster cohesion: fraction of SIMILAR_TO pairs present
	Sep     int     // number of distinct community ids spanned by the members
	Gap     float64 // derivation gap (computed in Task 8; zero until then)
	Spec    float64 // specificity signal (computed in Task 8; zero until then)
	Members int     // number of member facts in the seed set
	// Novelty holds §8's per-seed-set novelty signals, for `calibrate bridges`.
	// Diagnostic only: bridgeQ does not read them and no gate consults them.
	Novelty NoveltySignals
}

// NoveltySignals are blueprint §8's novelty measures for one bridge seed set.
//
// SEED-SET properties, computed at scoring time. §8 also asks for SURVIVAL,
// which is deliberately NOT here: survival is a property of a fact that does
// not exist yet when a candidate is scored, so a field for it would be filled
// in before there was anything to survive (Phase-4 Q4 ruling). It is reported
// separately, against discovered facts, by the survival report.
type NoveltySignals struct {
	// VectorsRead reports whether a vector source was available. Without it
	// SeedCos and OverDedup are not computed, and their zeros mean "unknown"
	// rather than "zero" — opposite findings that must not share a shape.
	VectorsRead bool
	// SeedCos is the mean body cosine over member pairs. Low means the members
	// are textually unalike, which on the far lane is the point.
	SeedCos float64
	// EntityJaccard is the mean entity-set overlap over member pairs. Computed
	// whether or not vectors are available.
	EntityJaccard float64
	// DedupKnown reports that the corpus's dedup threshold was resolved. When
	// false, OverDedup is NOT computed and its zero means "unknown" — the same
	// principle as VectorsRead, applied to the other input the signal needs.
	// A silently-defaulted threshold would be worse than no number: the
	// defaults are nomic's and every corpus in this campaign is embeddinggemma,
	// whose Dedup is 0.82 against the default 0.92.
	DedupKnown bool
	// OverDedup is the fraction of member pairs at or above the dedup
	// threshold.
	//
	// READ IT AS A FLOOR, NOT AN ALL-CLEAR. The dedup gate catches VERBATIM
	// duplicates only — semantic restatements sail past it, which is why
	// discovery's novelty rests on the agent's default-skip rather than on the
	// gate (gotcha 90d69628). So OverDedup > 0 does mean this group contains a
	// near-copy; OverDedup == 0 does NOT mean the members are saying different
	// things.
	OverDedup float64
}

// QualityConfig holds the tunable knobs for the bridge quality scorer. Fields
// are exported so the calibrate tool can construct and pass instances across
// the package boundary without using internal accessor methods.
type QualityConfig struct {
	CohFloor     float64 // minimum cohesion to pass the gate
	QualityFloor float64 // minimum Q to be kept (0 = accept all gate-passing sets)
	WCoh         float64 // weight for the cohesion term
	WGap         float64 // weight for the derivation-gap term
	WSpec        float64 // weight for the specificity term
	MaxMembers   int     // maximum number of members; larger sets are gated out
}

// cohesion returns the intra-cluster cohesion of members in g — the fraction
// of unordered pairs that share a SIMILAR_TO edge. It delegates entirely to
// g.Density so the math lives in one place.
func cohesion(members []string, g store.SimilarityGraph) float64 {
	return g.Density(members)
}

// separation counts the number of DISTINCT community ids spanned by members.
// A member absent from clusterOf is treated as its own unique community (it
// is keyed on the path string so two different absent paths each contribute
// one community and an absent path never collides with a real community id).
func separation(members []string, clusterOf map[string]int) int {
	if len(members) == 0 {
		return 0
	}
	seenIDs := make(map[int]struct{})
	seenUnknown := make(map[string]struct{})
	for _, m := range members {
		id, known := clusterOf[m]
		if known {
			seenIDs[id] = struct{}{}
		} else {
			seenUnknown[m] = struct{}{}
		}
	}
	return len(seenIDs) + len(seenUnknown)
}

// derivationGap returns the fraction of unordered member pairs that are NOT
// linked by a DERIVED_FROM edge in either direction. A pair (a,b) is linked
// iff a ∈ revdeps(b) OR b ∈ revdeps(a), where revdeps(x) is obtained from
// idx.ReverseDependentPaths. Reverse-dependency sets are memoized: each
// distinct member path is queried at most once. Returns 0 for fewer than 2
// members. Propagates the first error from ReverseDependentPaths.
func derivationGap(ctx context.Context, members []string, idx SearchQuery) (float64, error) {
	if len(members) < 2 {
		return 0, nil
	}

	// Deduplicate members so we fetch revdeps at most once per path.
	seen := make(map[string]struct{}, len(members))
	unique := members[:0:0] // same backing array, zero length
	for _, m := range members {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			unique = append(unique, m)
		}
	}

	// Fetch revdeps for every distinct member — one call each, no more.
	memo := make(map[string]map[string]struct{}, len(unique))
	for _, m := range unique {
		revdeps, err := idx.ReverseDependentPaths(ctx, m)
		if err != nil {
			return 0, err
		}
		memo[m] = revdeps
	}

	// Count unlinked pairs over the original (possibly duplicate-containing)
	// member slice; pair (i,j) with i<j.
	total := len(members)
	totalPairs := total * (total - 1) / 2
	unlinked := 0
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			a, b := members[i], members[j]
			_, aInRevdepsB := memo[b][a]
			_, bInRevdepsA := memo[a][b]
			if !aInRevdepsB && !bInRevdepsA {
				unlinked++
			}
		}
	}

	return float64(unlinked) / float64(totalPairs), nil
}

// specificity returns the raw token rarity signal for a single token: 1/df
// where df is the document frequency of the token on the given branch/kind.
// df=0 is treated as 1 (via max) so the result is always in (0, 1].
//
// Cross-bridge normalization is the calibrate tool's job. In Phase 3 a token
// is always supplied; the token-optional case (Phase 5) is handled by the
// caller before reaching this function.
func specificity(ctx context.Context, branch, token, kind string, idx SearchQuery) (float64, error) {
	df, err := idx.TokenDF(ctx, branch, token, kind)
	if err != nil {
		return 0, err
	}
	return 1.0 / float64(max(df, 1)), nil
}

// scoreBridgeCandidate computes all quality components and the Q-score for a
// single bridge candidate. The SimilarityGraph g must be fetched by the caller
// (so callers that already hold the adjacency — e.g. Task 15 — avoid a
// double-fetch). Every index error from derivationGap or specificity is
// propagated immediately.
func scoreBridgeCandidate(
	ctx context.Context,
	paths []string,
	kind BridgeKind,
	token string,
	g store.SimilarityGraph,
	idx SearchQuery,
	branch string,
	clusterOf map[string]int,
	cfg QualityConfig,
) (BridgeComponents, float64, bool, error) {
	// Canonicalize token for domain bridges; use as-is for entity bridges.
	specToken := token
	if kind == BridgeDomain {
		specToken = store.CanonicalizeTag(token)
	}

	gap, err := derivationGap(ctx, paths, idx)
	if err != nil {
		return BridgeComponents{}, 0, false, err
	}

	spec, err := specificity(ctx, branch, specToken, string(kind), idx)
	if err != nil {
		return BridgeComponents{}, 0, false, err
	}

	comp := BridgeComponents{
		Coh:     cohesion(paths, g),
		Sep:     separation(paths, clusterOf),
		Gap:     gap,
		Spec:    spec,
		Members: len(paths),
	}
	q, kept := bridgeQ(comp, cfg)
	return comp, q, kept, nil
}

// bridgeQ computes the weighted quality score Q for a bridge seed set and
// reports whether it should be kept.
//
// Gate (checked first — all must pass):
//   - c.Coh >= cfg.CohFloor
//   - c.Sep >= 2  (bridges must span at least two communities)
//   - c.Members <= cfg.MaxMembers
//
// If any gate fails, returns (0, false).
// Otherwise: q = cfg.WCoh*c.Coh + cfg.WGap*c.Gap + cfg.WSpec*c.Spec.
// Returns (q, q >= cfg.QualityFloor).
func bridgeQ(c BridgeComponents, cfg QualityConfig) (float64, bool) {
	if c.Coh < cfg.CohFloor || c.Sep < 2 || c.Members > cfg.MaxMembers {
		return 0, false
	}
	q := cfg.WCoh*c.Coh + cfg.WGap*c.Gap + cfg.WSpec*c.Spec
	return q, q >= cfg.QualityFloor
}
