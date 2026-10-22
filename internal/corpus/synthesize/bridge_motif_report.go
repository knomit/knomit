package synthesize

// The motif axis's calibrate/measurement entry point. The report types and the
// enumeration live in bridge (bridge.MotifReportFromSeeds, the sibling of
// BridgeComponentReport); this file keeps only the seed pool, because that pool
// is the review strategy's own AcceptSeed and bridge cannot import the parent.

import (
	"context"

	"knomit/internal/corpus/synthesize/bridge"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/vocab"

	"knomit/internal/domain/fact"
)

// MotifComponentReport enumerates, lane-splits and scores every motif bridge
// candidate on a corpus, and reports the served set beside the dropped one.
//
// The seed pool is built with production's own projection
// (step.FactFromSearchResult) and its own filter (reviewStrategy.AcceptSeed),
// so the numbers Phase 4 sets constants on come from the population the engine
// serves. The enumeration itself is bridge.MotifReportFromSeeds.
//
// Read-only: it opens nothing, writes nothing, and plans no work.
func MotifComponentReport(
	ctx context.Context,
	idx *index.Facade,
	motifs *vocab.MotifIndex,
	abstraction *vocab.AbstractionIndex,
	branch string,
	localRepoID string,
	eff step.Effort,
	resolution float64,
	minCommunitySize int,
	neighborKinds []string,
	cfg bridge.QualityConfig,
) (bridge.MotifReport, error) {
	results, err := idx.Search(ctx, branch, index.SearchOptions{Limit: 100000})
	if err != nil {
		return bridge.MotifReport{}, err
	}
	var strat reviewStrategy
	seeds := make([]step.FactForLLM, 0, len(results))
	for _, r := range results {
		f := step.FactFromSearchResult(r)
		if !strat.AcceptSeed(f) {
			continue
		}
		seeds = append(seeds, step.FactsForLLM([]fact.Fact{f}, localRepoID)...)
	}
	return bridge.MotifReportFromSeeds(ctx, idx, motifs, abstraction, branch, seeds, eff,
		resolution, minCommunitySize, neighborKinds, cfg)
}
