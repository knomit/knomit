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

// computeWeight reads each source path from git, parses it, and returns a
// normalized evidence weight. Sources that fail to read or parse contribute
// nothing. Must be called before source facts are deleted.
func computeWeight(ctx context.Context, gs store.FactIndex, agentBranch string, sourcePaths []string) float64 {
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
	return DefaultWeightStrategy.Compute(srcs)
}
