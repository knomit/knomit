package synthesize

import "knomit/internal/store"

// BridgeComponents holds the raw signal values computed for a bridge seed set.
// All fields are exported so the calibrate tool (a later Phase-3 task) can
// inspect them across the package boundary.
type BridgeComponents struct {
	Coh     float64 // intra-cluster cohesion: fraction of SIMILAR_TO pairs present
	Sep     int     // number of distinct community ids spanned by the members
	Gap     float64 // derivation gap (computed in Task 8; zero until then)
	Spec    float64 // specificity signal (computed in Task 8; zero until then)
	Members int     // number of member facts in the seed set
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
