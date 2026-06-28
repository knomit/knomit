package synthesize

import (
	"context"
	"sort"

	"knomit/internal/store"
)

// ScoredBridge is a single bridge candidate with its Q-score components and
// the gate/keep decision. Exported so the calibrate tool can consume it across
// the package boundary.
type ScoredBridge struct {
	// Token is the shared structural label (entity or domain name) that links
	// the member facts.
	Token string
	// Kind reports whether Token is a domain or entity bridge.
	Kind BridgeKind
	// Members holds the file paths of the member facts in the seed set.
	Members []string
	// Comp contains the raw signal values for the bridge.
	Comp BridgeComponents
	// Q is the weighted quality score (0 when gated out).
	Q float64
	// Kept is true when the bridge passes all gates and Q >= cfg.QualityFloor.
	Kept bool
}

// BridgeComponentReport enumerates the bridge candidates via
// enumerateBridgeCandidates for the given parameters, scores each with the Q
// components (cohesion, separation, derivation gap, specificity), and returns
// all candidates sorted by Q descending then Token ascending. At EffortNormal,
// returns (nil, nil) immediately (no candidates, no scoring calls).
//
// This is a calibrate/dev tool entrypoint: it is NOT wired into any
// production pipeline. Load the pool via Search (same as hypothesize.go) so
// the pool is mockable from tests.
//
// Errors from Search, CachedClusterFacts, SimilarityAdjacency, derivationGap,
// or specificity are propagated immediately.
func BridgeComponentReport(
	ctx context.Context,
	idx store.SearchIndex,
	branch string,
	kind BridgeKind,
	eff Effort,
	resolution float64,
	minCommunitySize int,
	cfg QualityConfig,
) ([]ScoredBridge, error) {
	// 1. Load pool via Search, mirroring hypothesize.go:138-150.
	results, err := idx.Search(ctx, branch, store.SearchOptions{
		IncludeTypes: []string{"synthesis"},
		Limit:        100000,
	})
	if err != nil {
		return nil, err
	}
	seeds := make([]factForLLM, 0, len(results))
	for _, r := range results {
		seeds = append(seeds, factForLLM{
			File:       r.Path,
			Title:      r.Title,
			Body:       r.Body,
			Type:       r.Type,
			Domain:     r.Domain,
			Entities:   r.Entities,
			Confidence: r.Confidence,
			Sources:    r.Sources,
			Origin:     r.Origin,
		})
	}

	// 2. Cluster the pool to get community assignments.
	cr, err := idx.CachedClusterFacts(ctx, branch, resolution, minCommunitySize)
	if err != nil {
		return nil, err
	}
	clusterOf := cr.ClusterOf()

	// 3. Enumerate bridge candidates (same engine the production pipeline uses).
	// At EffortNormal, discovery is disabled — return early without scoring.
	if !eff.Discovers() {
		return nil, nil
	}
	cands := enumerateBridgeCandidates(seeds, cr, kind, ScopeFilter{})
	if len(cands) == 0 {
		return nil, nil
	}

	// 4. Score each candidate.
	out := make([]ScoredBridge, 0, len(cands))
	for _, cand := range cands {
		paths := make([]string, 0, len(cand.Members))
		for _, m := range cand.Members {
			paths = append(paths, m.File)
		}

		g, err := idx.SimilarityAdjacency(ctx, paths)
		if err != nil {
			return nil, err
		}

		comp, q, kept, err := scoreBridgeCandidate(ctx, paths, cand.Kind, cand.Token, g, idx, branch, clusterOf, cfg)
		if err != nil {
			return nil, err
		}

		out = append(out, ScoredBridge{
			Token:   cand.Token,
			Kind:    cand.Kind,
			Members: paths,
			Comp:    comp,
			Q:       q,
			Kept:    kept,
		})
	}

	// 5. Sort by Q desc, then Token asc for deterministic output.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Q != out[j].Q {
			return out[i].Q > out[j].Q
		}
		return out[i].Token < out[j].Token
	})

	return out, nil
}
