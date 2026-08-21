package synthesize

import (
	"context"
	"time"

	"knomit/internal/store"
)

// The consolidation-scope fix.
//
// Prune IS consolidation with a judge — it replaces N facts with one newly
// authored body under an explicit fidelity contract — but its judge only ever
// sees facts that landed in the same cluster. Restatements whose halves cluster
// apart are judged by nothing and live forever
// (gotchas/synthesize/prune-scope/c40d6748).
//
// This file gives that judge a bounded, CORPUS-WIDE shortlist of candidate
// pairs, built on the abstraction axis (title-only embeddings) rather than on
// clustering. Nothing here is configurable: what a corpus spends is decided by
// its own data — the ranking is a percentile of its own pair-similarity
// distribution, and the budget is funded or defunded by its own judge's
// verdicts.

// titleBackfillBudget is a LATENCY BUDGET (a resource constant, not a claim
// about any corpus): the wall-clock one review session will spend embedding
// fact titles before getting on with the work the caller actually asked for.
//
// A large corpus therefore reaches full coverage across several sessions rather
// than stalling the first one for a minute. Partial coverage is reported in the
// session's health output, because a silently partial axis reads as "nothing to
// find" — the failure mode this area keeps hitting is caps that nothing reports.
const titleBackfillBudget = 15 * time.Second

// titleBackfillBatch is a THROUGHPUT BUDGET: how many titles go to the embedder
// per call. Batching amortizes ONNX setup, and since the clock is only checked
// between batches it also bounds how far a session can overrun the latency
// budget above.
const titleBackfillBatch = 32

// ensureTitleVectors fills the abstraction axis for live epistemic facts that
// lack a title vector, bounded by budget. It returns coverage (have, total)
// after the attempt, whether or not the budget ran out.
//
// Watermark-incremental BY CONSTRUCTION: facts rows are content-addressed, so
// an edited fact is a new row with no vector, and "rows lacking a vector" is
// exactly the delta. There is no watermark column here and nothing to migrate.
//
// A repo with no embedder (read-only tooling, tests) is a no-op rather than an
// error: the axis simply stays empty and the shortlist finds nothing.
func ensureTitleVectors(ctx context.Context, d Deps, branch string, budget time.Duration) (int, int, error) {
	emb := d.RI.Embedder()
	if emb == nil {
		return 0, 0, nil
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		targets, err := d.Abstraction.LiveFactsMissingTitleVector(ctx, branch, titleBackfillBatch)
		if err != nil {
			return 0, 0, wrapf(reviewTool, err, "title backfill: list missing")
		}
		if len(targets) == 0 {
			break
		}
		titles := make([]string, len(targets))
		for i, t := range targets {
			titles[i] = t.Title
		}
		// EmbedShortStrings, never EmbedDocument: a title is a few words, and
		// the short-string template is the rendering those were measured under
		// (embeddings.Model.ShortStringTemplate).
		vecs, err := emb.EmbedShortStrings(ctx, titles)
		if err != nil {
			return 0, 0, wrapf(reviewTool, err, "title backfill: embed")
		}
		if len(vecs) != len(targets) {
			return 0, 0, errf(reviewTool, "title backfill: embedder returned %d vectors for %d titles", len(vecs), len(targets))
		}
		out := make([]store.TitleVector, 0, len(targets))
		for i, t := range targets {
			out = append(out, store.TitleVector{FactID: t.FactID, Path: t.Path, Vec: vecs[i]})
		}
		if err := d.Abstraction.PutTitleVectors(ctx, out); err != nil {
			return 0, 0, wrapf(reviewTool, err, "title backfill: store")
		}
	}
	return d.Abstraction.TitleVectorCoverage(ctx, branch)
}
