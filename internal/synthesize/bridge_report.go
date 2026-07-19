package synthesize

import (
	"context"
	"sort"
	"strings"

	"knomit/internal/fact"
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

// BridgeReportPool selects which production seed-builder BridgeComponentReport
// mirrors. The two production pipelines build materially different pools —
// diagnosing against the wrong one silently validates the wrong thing.
type BridgeReportPool string

const (
	// PoolBackward mirrors hypothesize.go's real seed pool (IncludeTypes=
	// synthesis only) — BuildBackwardBridges' actual production input. This
	// is the historical default and what every bridge diagnostic before this
	// pool-mode option was added was implicitly measuring.
	PoolBackward BridgeReportPool = "backward"
	// PoolForward mirrors review.go's dirtyFacts real seed pool (IncludeKinds
	// =epistemic, no type restriction — authored, distilled, discovered,
	// synthesis, observation, hypothesis, etc. all included) — the actual
	// production input to buildScoredBridges via knomit_review. Materially
	// larger and differently shaped than PoolBackward; see
	// .claude/plans/yake-df-gate-forward-vs-backward-pool-report.md.
	PoolForward BridgeReportPool = "forward"
)

// BridgeComponentReport enumerates the bridge candidates via
// enumerateBridgeCandidates for the given parameters, scores each with the Q
// components (cohesion, separation, derivation gap, specificity), and returns
// all candidates sorted by Q descending then Token ascending. At EffortNormal,
// returns (nil, nil) immediately (no candidates, no scoring calls).
//
// pool selects which production pipeline's real seed-building filter to
// mirror (see BridgeReportPool) — "" defaults to PoolBackward for backward
// compatibility with callers written before this parameter existed.
//
// This is a calibrate/dev tool entrypoint: it is NOT wired into any
// production pipeline. Load the pool via Search (same as hypothesize.go /
// review.go) so the pool is mockable from tests.
//
// Errors from Search, ScopedCluster, SimilarityAdjacency, derivationGap,
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
	pool BridgeReportPool,
) ([]ScoredBridge, error) {
	// 1. Load pool via Search, mirroring either hypothesize.go:138-150
	// (PoolBackward) or review.go's dirtyFacts full-scan path (PoolForward).
	opts := store.SearchOptions{Limit: 100000}
	if pool == PoolForward {
		opts.IncludeKinds = []string{string(fact.Epistemic)}
	} else {
		opts.IncludeTypes = []string{"synthesis"}
	}
	results, err := idx.Search(ctx, branch, opts)
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

	// 2. Cluster the pool to get community assignments. Same in-process scoped
	// clustering the production pipeline uses (ScopedCluster → Louvain over
	// idx.SubgraphEdges), adapted to ClusterResult for the bridge engine.
	groups, err := ScopedCluster(ctx, seeds, idx, resolution, minCommunitySize, func(ProgressEvent) {}, branch)
	if err != nil {
		return nil, err
	}
	cr := clusterResultFromGroups(groups)
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

		// Reshape oversized groups to the most cohesive cross-community
		// subset, exactly mirroring buildScoredBridges (bridge.go). Without
		// this step, candidates over cfg.MaxMembers were dropped outright
		// instead of trimmed, silently under-reporting keyword-bridge yield
		// on pools large enough to produce them (never visible on a small
		// pool, common on a large one) — see
		// .claude/plans/yake-forward-vs-backward-pool-report.md.
		if len(paths) > cfg.MaxMembers {
			sub := reshapeCohesiveSubset(paths, g, clusterOf, cfg.CohFloor, cfg.MaxMembers)
			if sub == nil {
				continue // no cohesive cross-community seam — drop candidate
			}
			paths = sub
		}

		var specOvr float64
		if cand.Kind == BridgeKeyword {
			df := 0
			lower := strings.ToLower(cand.Token)
			for _, f := range seeds {
				if strings.Contains(strings.ToLower(f.Body), lower) {
					df++
				}
			}
			var keepDF bool
			specOvr, keepDF = keywordDFGate(df, len(seeds), cfg)
			if !keepDF {
				continue
			}
		}
		comp, q, kept, err := scoreBridgeCandidate(ctx, paths, cand.Kind, cand.Token, g, idx, branch, clusterOf, cfg, specOvr)
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
