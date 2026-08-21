package synthesize

import (
	"context"
	"math"
	"slices"
	"sync/atomic"
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

// pairNeighbourK is how many title-neighbours are retained per fact — a
// STRUCTURAL BUDGET with system precedent (store.knnK = 10 for SIMILAR_TO),
// never a claim about how many restatements a corpus contains. It bounds the
// standing cache at K·N rows and per-session work at O(Δ·K).
const pairNeighbourK = 10

// neighbourQueryCount counts KNN lookups so tests can assert that per-session
// work is proportional to what CHANGED rather than to corpus size.
var neighbourQueryCount atomic.Int64

// refreshRestatementShortlist brings the standing candidate cache up to date.
//
// Pairing is CORPUS-WIDE and session-independent: the session's scope and the
// session's clusters play no part here (scope is applied at emission,
// cluster co-membership at selection). That separation is the point — the
// target population is precisely the pairs a scoped view never co-presents, so
// restricting the pairing to a session's own view would rebuild the blindness
// this exists to fix.
//
// NO clustering runs in this path. The neighbour lookup is the same KNN the
// similarity graph uses.
func refreshRestatementShortlist(ctx context.Context, d Deps, branch string, dedupThreshold float64) error {
	live, err := d.Abstraction.LiveEpistemicFacts(ctx, branch)
	if err != nil {
		return wrapf(reviewTool, err, "shortlist: live facts")
	}
	cached, err := d.Abstraction.CachedPairFactIDs(ctx, branch)
	if err != nil {
		return wrapf(reviewTool, err, "shortlist: cached fact ids")
	}

	// The delta is the symmetric difference. An edit shows up as one added id
	// and one dropped id, because facts rows are content-addressed — which is
	// why nothing here needs a watermark.
	var added []int64
	for id := range live {
		if _, ok := cached[id]; !ok {
			added = append(added, id)
		}
	}
	var dropped []int64
	for id := range cached {
		if _, ok := live[id]; !ok {
			dropped = append(dropped, id)
		}
	}
	if len(added) == 0 && len(dropped) == 0 {
		return nil
	}
	// Deterministic order so two runs over the same delta produce the same
	// cache, and so a test can talk about "the first fact".
	slices.Sort(added)
	slices.Sort(dropped)

	var candidates []store.RestatementPair
	for _, id := range added {
		neighbourQueryCount.Add(1)
		neighbours, err := d.Abstraction.TopTitleNeighbours(ctx, branch, id, pairNeighbourK)
		if err != nil {
			return wrapf(reviewTool, err, "shortlist: neighbours for fact %d", id)
		}
		for _, n := range neighbours {
			candidates = append(candidates, newRestatementPair(id, live[id], n))
		}
	}

	kept, err := filterByBlendedCosine(ctx, d, candidates, dedupThreshold)
	if err != nil {
		return err
	}
	return d.Abstraction.ReplaceRestatementPairs(ctx, branch, append(dropped, added...), kept, added)
}

// newRestatementPair canonicalises a pair so A-B and B-A are one row.
func newRestatementPair(id int64, path string, n store.TitleNeighbour) store.RestatementPair {
	p := store.RestatementPair{
		APath: path, BPath: n.Path,
		AFactID: id, BFactID: n.FactID,
		TitleCos: n.Similarity,
	}
	if p.BPath < p.APath {
		p.APath, p.BPath = p.BPath, p.APath
		p.AFactID, p.BFactID = p.BFactID, p.AFactID
	}
	return p
}

// filterByBlendedCosine drops pairs whose BLENDED (title+body) vectors already
// sit at or above the model's calibrated dedup threshold.
//
// Those pairs are not restatements the judge needs to see: mergeFacts already
// merges them mechanically, so spending a judge slot on one is pure waste. The
// threshold is the active model's own calibrated value
// (internal/embeddings/params), not a constant invented here — the shortlist
// contributes no absolute cosine of its own.
//
// Vectors come from the stored facts_vec rows. Nothing is re-embedded.
func filterByBlendedCosine(ctx context.Context, d Deps, pairs []store.RestatementPair, dedupThreshold float64) ([]store.RestatementPair, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	idSet := map[int64]struct{}{}
	for _, p := range pairs {
		idSet[p.AFactID] = struct{}{}
		idSet[p.BFactID] = struct{}{}
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	vecs, err := d.Abstraction.BodyVectorsByFactID(ctx, ids)
	if err != nil {
		return nil, wrapf(reviewTool, err, "shortlist: body vectors")
	}

	out := make([]store.RestatementPair, 0, len(pairs))
	for _, p := range pairs {
		a, aok := vecs[p.AFactID]
		b, bok := vecs[p.BFactID]
		if !aok || !bok {
			// A fact with no stored vector cannot be scored. Keeping it would
			// mean guessing whether dedup would already have caught the pair.
			continue
		}
		if cosine(a, b) >= dedupThreshold {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// cosine of two stored vectors. They are L2-normalized at embed time, so this
// is a dot product; the norms are recomputed anyway rather than assumed,
// because a donated vector (WithPrecomputedEmbeddings) is only validated for
// dimension.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na <= 1e-12 || nb <= 1e-12 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
