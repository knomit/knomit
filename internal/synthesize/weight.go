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

// computeTransfer reads each source path from git once and returns the three
// numbers a TRANSFER-type write needs: the normalized evidence weight, the
// pooled corroboration count, and how many sources were actually readable.
//
// Transfer vs share is the distinction that decides whether pooling is correct
// at all (spec/mbekg.md §5.1). A merge DELETES the facts it consolidates, so
// their corroborations have nowhere else to live and must move to the output
// or be lost — summing is exactly §2.2's semantics. A derivation (distill,
// discover, reflect-propose) leaves its sources alive holding their own
// counts, so summing there would record one observation in two live facts at
// once and inflate multiplicatively on every review cycle. Only merges call
// this; derivations call computeWeight and set sources to 1.
//
// The pooled set is exactly the set weighed: sources that fail to read or
// parse contribute to neither, and hypothesis-typed sources are skipped
// because a conjecture corroborates nothing (§5.2). Letting the two numbers
// disagree would have them describe different evidence.
//
// Must be called before the source facts are deleted — computing afterwards
// reads nothing and reports a fact resting on no evidence at all. readable is
// returned so a destructive caller can tell "these facts had no corroborations"
// apart from "I could not read the facts I am about to delete".
func computeTransfer(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) (weight float64, pooled int, readable int) {
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
	for _, s := range srcs {
		pooled += s.Sources
	}
	if pooled < derivedSourcesFloor {
		pooled = derivedSourcesFloor
	}
	return DefaultWeightStrategy.Compute(srcs), pooled, len(srcs)
}

// computeWeight returns only the evidence weight, for SHARE-type writes: a
// derivation whose sources stay alive sets its own sources to 1 and has no
// count to pool (see computeTransfer for why).
func computeWeight(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) float64 {
	w, _, _ := computeTransfer(ctx, gs, agentBranch, sourcePaths)
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
