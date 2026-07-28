package synthesize

import (
	"context"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// SourceWeight holds the confidence and sources count from a single source fact,
// used to compute a normalized evidence weight for synthesized facts.
type SourceWeight struct {
	Confidence float64
	Sources    int
}

// WeightStrategy computes a normalized evidence weight in [0, 1) from a set
// of source facts. Implement this interface to add alternative formulas.
type WeightStrategy interface {
	Compute(sources []SourceWeight) float64
}

// SumProductNorm implements WeightStrategy using sum(c*s) / (sum(c*s) + 1).
// This maps [0, ∞) → [0, 1) with diminishing returns as evidence accumulates.
type SumProductNorm struct{}

// Compute returns sum(confidence_i * sources_i) / (sum + 1).
func (SumProductNorm) Compute(srcs []SourceWeight) float64 {
	if len(srcs) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range srcs {
		sum += s.Confidence * float64(s.Sources)
	}
	return sum / (sum + 1.0)
}

// DefaultWeightStrategy is used by ApplyPruneDecisions and ApplyDistillDecisions.
// Swap to change the formula globally.
var DefaultWeightStrategy WeightStrategy = SumProductNorm{}

// derivedSourcesFloor is the sources count a derived fact carries when it
// pools nothing — no local refs, or every ref unreadable or hypothesis-typed.
// A synthesis is itself one act of corroboration, and letting the count reach
// 0 would erase the fact from every downstream weight: Compute multiplies by
// sources, so a 0-source ref contributes exactly nothing no matter how
// confident it is.
const derivedSourcesFloor = 1

// computeEvidence reads each source path from git once and returns both
// derived quantities a synthesized fact needs: the normalized evidence weight
// and the pooled corroboration count.
//
// They share a pass because they must share a source set. spec/mbekg.md §2.2
// defines sources as "how many independent agents or observations produced
// this fact", so a derived fact's count is the sum of what it was built from —
// and any source excluded from the weight must be excluded from the sum for
// the same reason, or the two numbers describe different evidence. Sources
// that fail to read or parse contribute nothing to either; hypothesis-typed
// sources are skipped because a conjecture corroborates nothing (§5.2).
//
// Must be called before source facts are deleted — a merge that computed
// after deleting its inputs would read nothing and silently report a fact
// resting on no evidence at all.
func computeEvidence(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) (float64, int) {
	var srcs []SourceWeight
	for _, p := range sourcePaths {
		readResult, err := gs.ReadFact(ctx, agentBranch, p, nil)
		if err != nil {
			continue
		}
		f, err := fact.ParseFact(p, readResult.Content)
		if err != nil {
			continue
		}
		// Skip hypothesis-type sources — they carry uncertainty and should not
		// contribute to evidence weight for synthesized facts.
		if f.Type == fact.Hypothesis {
			continue
		}
		srcs = append(srcs, SourceWeight{Confidence: f.Confidence, Sources: f.Sources})
	}
	pooled := 0
	for _, s := range srcs {
		pooled += s.Sources
	}
	if pooled < derivedSourcesFloor {
		pooled = derivedSourcesFloor
	}
	return DefaultWeightStrategy.Compute(srcs), pooled
}

// computeWeight returns only the evidence weight, for callers that write no
// fact of their own and so have no sources count to set.
func computeWeight(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) float64 {
	w, _ := computeEvidence(ctx, gs, agentBranch, sourcePaths)
	return w
}

// ComputeEvidenceWeight is the exported entry to computeWeight. The learn
// handler uses it so a manually-written derived fact (one an agent marks as a
// machine origin after previewing discovery/synthesis proposals) carries the
// same provenance weight the auto-apply pipeline would have computed.
// sourcePaths must be local fact paths.
func ComputeEvidenceWeight(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) float64 {
	return computeWeight(ctx, gs, agentBranch, sourcePaths)
}
