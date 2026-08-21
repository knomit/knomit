package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

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

// ── selection ─────────────────────────────────────────────────────────────
//
// What a session spends is decided entirely by the corpus's own data: the
// ranking is a percentile of its own pair-similarity distribution, and the
// budget is funded or defunded by its own judge's verdicts. There is no
// threshold here to configure, and no constant that claims a fact about any
// corpus (MN13).

// maxShortlistItems is a JUDGE-SLOT BUDGET: the most restatement pairs one
// review session will ever put in front of the judge, whatever the corpus
// looks like. It bounds what a corpus where title similarity does not
// discriminate can waste before its own verdicts defund it.
const maxShortlistItems = 8

// shortlistPerMille scales that budget to corpus size — five slots per thousand
// facts. Also a JUDGE-SLOT BUDGET: it allocates spend, it does not claim a
// restatement RATE. At small N it evaluates to 0-1, which is the cold-start
// posture: a brand-new corpus risks a slot or two before any mechanism here has
// learned anything about it.
const shortlistPerMille = 5

// throttleWindow is how many shortlist verdicts the trailing merge-rate is
// measured over; throttleMinVerdicts is how much evidence must accumulate
// before a corpus may defund itself. Both are PATIENCE BUDGETS, trading wasted
// judge slots against how fast a corpus can turn its own shortlist off.
const (
	throttleWindow      = 10
	throttleMinVerdicts = 5
)

// shortlistOverfetch is how many ranked pairs are considered per funded slot,
// so per-candidate exclusions cannot silently shrink a funded batch. A
// RESOURCE BUDGET on a SQL read, nothing more.
const shortlistOverfetch = 4

// Throttle states, reported in health output.
const (
	throttleOptimistic = "optimistic" // no history yet — the cap bounds the downside
	throttleFunded     = "funded"     // the judge has merged something recently
	throttleDefunded   = "defunded"   // enough judged, none merged: stop spending
)

// restatementClusterKeyPrefix marks a prune work item as shortlist-originated.
// Verdict attribution reads it, so an ordinary cluster prune can never be
// mistaken for evidence that the shortlist is earning its slots.
const restatementClusterKeyPrefix = "restate-"

// restatementPriority places shortlist items inside prune's positive band but
// below every real cluster (a prune cluster always has at least two facts, so
// its priority is >= 2). Ordinary cluster consolidation — the bulk of the work
// and the part that never needed this mechanism — therefore always runs first.
const restatementPriority = 1.5

// restatementHealth is what a session reports about the axis and the shortlist.
// Every field is OBSERVABILITY: descriptors of this corpus, read by no branch.
// A corpus descriptor that decided anything would be the corpus-property
// constant this design exists without.
type restatementHealth struct {
	Coverage       float64 // fraction of live epistemic facts on the axis
	StandingPairs  int
	TailP99        float64
	TailP999       float64
	OperatingPoint float64 // title-cos of the last selected pair, in THIS repo
	Emitted        int
	MergeRate      float64
	ThrottleState  string
}

// shortlistBudget is how many judge slots this session may spend.
func shortlistBudget(n int) int {
	b := n * shortlistPerMille / 1000
	if b > maxShortlistItems {
		return maxShortlistItems
	}
	return b
}

// throttleState reads the corpus's own verdict history: optimistic with no
// history, defunded once enough shortlist pairs have been judged and none
// merged, funded again the moment one merges.
func throttleState(verdicts []store.RestatementVerdict) (float64, string) {
	if len(verdicts) == 0 {
		return 0, throttleOptimistic
	}
	merges := 0
	for _, v := range verdicts {
		if v.Merged {
			merges++
		}
	}
	rate := float64(merges) / float64(len(verdicts))
	if merges == 0 && len(verdicts) >= throttleMinVerdicts {
		return rate, throttleDefunded
	}
	return rate, throttleFunded
}

// selectRestatementCandidates picks this session's pairs off the standing
// shortlist.
//
// clusters is what dedupCluster produced for THIS session — used as a
// membership check, never re-clustered: a pair already sitting in one cluster
// is a pair prune sees anyway, so spending a shortlist slot on it is spending
// twice for one judgement.
func selectRestatementCandidates(ctx context.Context, d Deps, branch string, clusters [][]factForLLM, n int) ([]store.RestatementPair, restatementHealth, error) {
	var h restatementHealth

	// Coverage is read here rather than passed in, so every caller that builds
	// health gets a complete one. A health block that silently reports 0%
	// because its caller forgot to fill a field is worse than no health block.
	if have, total, cerr := d.Abstraction.TitleVectorCoverage(ctx, branch); cerr == nil && total > 0 {
		h.Coverage = float64(have) / float64(total)
	}

	stats, err := d.Abstraction.RestatementPairStats(ctx, branch)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: stats")
	}
	h.StandingPairs, h.TailP99, h.TailP999 = stats.Count, stats.P99, stats.P999

	verdicts, err := d.Abstraction.RecentRestatementVerdicts(ctx, branch, throttleWindow)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: verdicts")
	}
	h.MergeRate, h.ThrottleState = throttleState(verdicts)

	budget := shortlistBudget(n)
	if h.ThrottleState == throttleDefunded || budget == 0 {
		return nil, h, nil
	}

	kept, err := d.Abstraction.KeptPairFactIDs(ctx, branch)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: kept pairs")
	}

	// Over-fetch: the exclusions below are decided per candidate, and cutting to
	// the budget first would let one excluded pair silently shrink a batch the
	// throttle had funded.
	raw, err := d.Abstraction.RestatementPairsByRank(ctx, branch, budget*shortlistOverfetch)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: rank")
	}

	coGrouped := clusterCoMembership(clusters)
	var out []store.RestatementPair
	for _, p := range raw {
		if _, ok := coGrouped[pathPairKey(p.APath, p.BPath)]; ok {
			continue
		}
		if _, ok := kept[store.FactIDPairKey(p.AFactID, p.BFactID)]; ok {
			continue
		}
		if !d.Scope.IsEmpty() && !pairTouchesScope(ctx, d, branch, p) {
			continue
		}
		out = append(out, p)
		if len(out) == budget {
			break
		}
	}
	if len(out) > 0 {
		// The operating point is not a threshold anyone chose: it is whatever
		// absolute cosine the last funded pair happens to sit at in THIS repo.
		// Reported because it is a corpus fingerprint — the same code on
		// another corpus prints a different number.
		h.OperatingPoint = out[len(out)-1].TitleCos
	}
	h.Emitted = len(out)
	return out, h, nil
}

// clusterCoMembership is the set of pairs this session's own dedupCluster
// already places in one cluster — the exact "prune already sees them together"
// exclusion. A membership check over clusters the caller already computed; no
// clustering runs here.
func clusterCoMembership(clusters [][]factForLLM) map[string]struct{} {
	out := map[string]struct{}{}
	for _, c := range clusters {
		for i := range c {
			for j := i + 1; j < len(c); j++ {
				out[pathPairKey(c[i].File, c[j].File)] = struct{}{}
			}
		}
	}
	return out
}

// pathPairKey is the canonical key for an unordered pair of paths.
func pathPairKey(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return a + "\x00" + b
}

// pairTouchesScope reports whether either endpoint falls inside a scoped
// session's filter.
//
// The standing shortlist stays corpus-wide — that is the whole point, since the
// target population is pairs a scoped view never co-presents — but a session
// asked to work on one area should not spend its judge slots on two facts that
// are both somewhere else. A fully out-of-scope pair simply waits for an
// unscoped session.
func pairTouchesScope(ctx context.Context, d Deps, branch string, p store.RestatementPair) bool {
	for _, path := range []string{p.APath, p.BPath} {
		f, err := d.Search.GetByPath(ctx, branch, path)
		if err != nil || f == nil {
			continue
		}
		if d.Scope.Matches(f.Domain, f.Entities) {
			return true
		}
	}
	return false
}

// enqueueRestatementItems turns selected pairs into ordinary prune work items.
//
// Ordinary is the point: the prune prompt ships facts beside itself rather than
// interpolating them, so a two-fact item is shape-identical to a cluster item
// and needs no prompt, schema, or apply-path change. The judge that already
// knows how to merge under a fidelity contract gets to apply it to pairs it
// previously never saw.
func enqueueRestatementItems(ctx context.Context, d Deps, sess *store.PipelineSession, branch string, pairs []store.RestatementPair) error {
	for i, p := range pairs {
		facts := make([]factForLLM, 0, 2)
		for _, path := range []string{p.APath, p.BPath} {
			f, err := d.Search.GetByPath(ctx, branch, path)
			if err != nil {
				return wrapf(reviewTool, err, "shortlist: read %s", path)
			}
			if f == nil {
				break // raced a retraction; drop the pair rather than half-ship it
			}
			facts = append(facts, factForLLM{
				File:       f.Path,
				Title:      f.Title,
				Body:       f.Body,
				Type:       f.Type,
				Domain:     f.Domain,
				Entities:   f.Entities,
				Confidence: f.Confidence,
				Sources:    f.Sources,
				Origin:     f.Origin,
			})
		}
		if len(facts) != 2 {
			continue
		}
		factsJSON, err := json.Marshal(facts)
		if err != nil {
			return wrapf(reviewTool, err, "shortlist: marshal pair %d", i)
		}
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "prune",
			ClusterKey: fmt.Sprintf("%s%d", restatementClusterKeyPrefix, i),
			FactsJSON:  string(factsJSON),
			Priority:   restatementPriority,
		}); err != nil {
			return wrapf(reviewTool, err, "shortlist: insert item %d", i)
		}
	}
	return nil
}

// planRestatementShortlist runs the whole phase-0 sequence for one session:
// top up the axis, refresh the standing pair cache, select what this corpus's
// own history says it may spend, and enqueue the result as prune items.
//
// Only an enqueue failure is fatal. Everything upstream of it degrades to "no
// candidates and a health line saying so": this mechanism is an ADDITION to
// consolidation, and a corpus whose axis cannot be built should still get its
// ordinary review rather than an error.
func planRestatementShortlist(ctx context.Context, d Deps, sess *store.PipelineSession, branch string, clusters [][]factForLLM, seeds int) error {
	_, total, err := ensureTitleVectors(ctx, d, branch, titleBackfillBudget)
	if err != nil {
		log.Warn().Err(err).Str("session", sess.ID).
			Msg("review: title backfill failed; skipping restatement shortlist")
		return nil
	}
	if total == 0 {
		return nil // empty corpus: every ratio is zero and there is nothing to do
	}

	dedupThreshold := store.EmbedderThresholds(d.RI.Embedder()).Dedup
	if err := refreshRestatementShortlist(ctx, d, branch, dedupThreshold); err != nil {
		log.Warn().Err(err).Str("session", sess.ID).
			Msg("review: restatement shortlist refresh failed; continuing without candidates")
		return nil
	}

	pairs, health, err := selectRestatementCandidates(ctx, d, branch, clusters, seeds)
	if err != nil {
		log.Warn().Err(err).Str("session", sess.ID).
			Msg("review: restatement selection failed; continuing without candidates")
		return nil
	}

	if err := enqueueRestatementItems(ctx, d, sess, branch, pairs); err != nil {
		return err
	}
	recordRestatementHealth(ctx, d, sess.ID, health)
	return nil
}

// ── health ────────────────────────────────────────────────────────────────

// healthLines renders the phase-0 observability block.
//
// Every line is a DESCRIPTOR derived from this corpus's own data, and no line
// is read by any branch in this package. The operating point in particular is
// printed precisely because it is a fingerprint: the same code on two corpora
// prints two different cosines, which is what it means for the selection to be
// a percentile of each repo's own distribution rather than a threshold someone
// picked (MN13).
func healthLines(h restatementHealth) []string {
	return []string{
		fmt.Sprintf("abstraction coverage: %.0f%% of live epistemic facts", h.Coverage*100),
		fmt.Sprintf("standing restatement pairs: %d (title-cos p99 %.3f, p99.9 %.3f)",
			h.StandingPairs, h.TailP99, h.TailP999),
		fmt.Sprintf("operating point: title-cos %.3f (this corpus, this session)", h.OperatingPoint),
		fmt.Sprintf("restatement candidates emitted: %d", h.Emitted),
		fmt.Sprintf("shortlist throttle: %s (trailing merge-rate %.0f%% over last %d judged)",
			h.ThrottleState, h.MergeRate*100, throttleWindow),
	}
}

// recordRestatementHealth stores the session's health lines so the result the
// caller receives can carry them. The engine is per-call stateless, so this
// cannot be kept in memory between planning and responding.
func recordRestatementHealth(ctx context.Context, d Deps, sessionID string, h restatementHealth) {
	encoded, err := json.Marshal(healthLines(h))
	if err != nil {
		return
	}
	if err := d.Pipeline.SetPipelineSessionHealth(ctx, sessionID, string(encoded)); err != nil {
		// Informational, like recordStats: losing a health line costs
		// visibility, never correctness.
		log.Warn().Err(err).Str("session", sessionID).Msg("review: could not record health lines")
	}
}

// sessionHealthLines decodes what recordRestatementHealth stored.
func sessionHealthLines(sess *store.PipelineSession) []string {
	if sess == nil || sess.Health == "" {
		return nil
	}
	var lines []string
	if err := json.Unmarshal([]byte(sess.Health), &lines); err != nil {
		return nil
	}
	return lines
}

// ── verdict attribution ───────────────────────────────────────────────────

// judgedPair is a shortlist pair as it stood when the judge was shown it: the
// two paths, and the fact ids of the exact versions.
type judgedPair struct {
	APath, BPath     string
	AFactID, BFactID int64
}

// resolveShortlistPair identifies the pair a shortlist-originated prune item
// put in front of the judge, capturing the fact ids of the versions shown.
// Returns nil for ordinary cluster items — those say nothing about whether the
// shortlist is earning its slots.
//
// Called BEFORE the decisions are applied, because applying them can rewrite a
// fact, and a rewritten fact is a new row with a new id.
func resolveShortlistPair(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem) *judgedPair {
	if !strings.HasPrefix(item.ClusterKey, restatementClusterKeyPrefix) {
		return nil
	}
	paths, err := itemInputPaths(item)
	if err != nil || len(paths) != 2 {
		return nil
	}
	live, err := d.Abstraction.LiveEpistemicFacts(ctx, sess.Branch)
	if err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: could not resolve shortlist verdict ids")
		return nil
	}
	ids := map[string]int64{}
	for id, path := range live {
		ids[path] = id
	}
	return &judgedPair{
		APath: paths[0], BPath: paths[1],
		AFactID: ids[paths[0]], BFactID: ids[paths[1]],
	}
}

// recordShortlistVerdict records what the judge did with a shortlist pair.
//
// "Merged" means the judge merged THE PAIR the shortlist put in front of it,
// not merely that the item produced some merge — on a two-fact item those
// coincide, but saying it precisely keeps the throttle measuring what it
// claims to measure.
func recordShortlistVerdict(ctx context.Context, d Deps, sess *store.PipelineSession, judged *judgedPair, res *PruneResult) {
	if judged == nil || res == nil {
		return
	}
	merged := false
	for _, m := range res.Merges {
		if pairCovered(m.Paths, judged.APath, judged.BPath) {
			merged = true
			break
		}
	}
	if err := d.Abstraction.RecordRestatementVerdict(ctx, sess.Branch, store.RestatementVerdict{
		APath: judged.APath, BPath: judged.BPath,
		AFactID: judged.AFactID, BFactID: judged.BFactID,
		Merged: merged, JudgedAt: time.Now().UTC(),
	}); err != nil {
		// Informational, like recordStats: the mutations are already committed,
		// and a lost verdict only makes the throttle slightly more optimistic.
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: could not record shortlist verdict")
	}
}

// pairCovered reports whether a merge entry covers both halves of the pair.
func pairCovered(mergePaths []string, a, b string) bool {
	var sawA, sawB bool
	for _, p := range mergePaths {
		switch p {
		case a:
			sawA = true
		case b:
			sawB = true
		}
	}
	return sawA && sawB
}
