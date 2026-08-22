package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
			out = append(out, store.TitleVector{FactID: t.FactID, Vec: vecs[i]})
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

// refreshStats reports what one refresh actually did. Returned rather than
// counted in a package variable: test instrumentation does not belong in
// production state, and a caller that wants to log the cost should be handed it.
type refreshStats struct {
	NeighbourQueries int // KNN lookups — the per-session work
	PairsAdded       int
	FactsRequeued    int  // partners re-scanned so an asymmetric discovery is not lost
	AxisComplete     bool // false while the backfill is still filling the axis
}

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
func refreshRestatementShortlist(ctx context.Context, d Deps, branch string, dedupThreshold float64) (refreshStats, error) {
	var stats refreshStats

	// Diff against facts that are actually ON THE AXIS. A fact with no title
	// vector has no neighbours to find, so counting it as covered would mark it
	// done and its KNN would never run — permanently, because the cache state
	// is what says "already covered". During a partial backfill that would
	// freeze the cache at whatever the first session managed.
	onAxis, err := d.Abstraction.LiveEpistemicFactsOnAxis(ctx, branch)
	if err != nil {
		return stats, wrapf(reviewTool, err, "shortlist: live facts on axis")
	}
	live, err := d.Abstraction.LiveEpistemicFacts(ctx, branch)
	if err != nil {
		return stats, wrapf(reviewTool, err, "shortlist: live facts")
	}
	cached, err := d.Abstraction.CachedPairFactIDs(ctx, branch)
	if err != nil {
		return stats, wrapf(reviewTool, err, "shortlist: cached fact ids")
	}

	// While the axis is incomplete, every on-axis fact is in play: see the
	// coverage note where the cache state is written.
	complete := len(onAxis) == len(live)

	// An edit shows up as one added id and one dropped id, because facts rows
	// are content-addressed — which is why nothing here needs a watermark.
	var added []int64
	for id := range onAxis {
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
		return stats, nil
	}
	stats.AxisComplete = complete

	// KNN is ASYMMETRIC: a standing pair may exist only because B's top-K
	// included A, and never the reverse. Dropping every pair that touches a
	// departed fact and re-scanning only the arrivals would therefore destroy
	// discoveries that nothing rediscovers. Re-scan the surviving partners too
	// — bounded at K per dropped fact.
	partners, err := d.Abstraction.PartnersOfFacts(ctx, branch, dropped)
	if err != nil {
		return stats, wrapf(reviewTool, err, "shortlist: partners of dropped facts")
	}
	requeue := append([]int64(nil), added...)
	for id := range partners {
		if _, stillOnAxis := onAxis[id]; stillOnAxis {
			requeue = append(requeue, id)
			stats.FactsRequeued++
		}
	}
	// Deterministic order so two runs over the same delta produce the same
	// cache, and so a test can talk about "the first fact".
	slices.Sort(requeue)
	requeue = slices.Compact(requeue)

	// Pairs the judge already declined must not be re-minted by this rescan.
	// The pair itself was deleted when the verdict landed; this is what keeps
	// it gone until one of its facts is edited (which makes it a new pair of
	// content-addressed ids, and therefore genuinely unseen).
	declined, err := d.Abstraction.KeptPairFactIDs(ctx, branch)
	if err != nil {
		return stats, wrapf(reviewTool, err, "shortlist: declined pairs")
	}

	var candidates []store.RestatementPair
	for _, id := range requeue {
		stats.NeighbourQueries++
		neighbours, err := d.Abstraction.TopTitleNeighbours(ctx, branch, id, pairNeighbourK)
		if err != nil {
			return stats, wrapf(reviewTool, err, "shortlist: neighbours for fact %d", id)
		}
		for _, n := range neighbours {
			if _, ok := declined[store.FactIDPairKey(id, n.FactID)]; ok {
				continue
			}
			candidates = append(candidates, newRestatementPair(id, onAxis[id], n))
		}
	}

	kept, unscorable, err := filterByBlendedCosine(ctx, d, candidates, dedupThreshold)
	if err != nil {
		return stats, err
	}
	stats.PairsAdded = len(kept)

	// Nothing is recorded as covered until the axis is COMPLETE.
	//
	// The cache state means "scanned against the whole corpus", and during a
	// backfill that is a lie: a fact scanned while half the corpus had no title
	// vector cannot have seen the partners embedded later, and KNN asymmetry
	// means the later fact's own scan may not find it either. Marking it
	// covered would make that a permanent hole rather than a delay. So while
	// coverage is partial, every on-axis fact is rescanned each session — the
	// pairs found so far are still offered, they are just not treated as final.
	// The cost is bounded to the fill period and vanishes the session coverage
	// closes.
	//
	// A fact whose blended vector could not be read was not really scanned
	// either, for the same reason and with the same consequence.
	var covered []int64
	if complete {
		covered = make([]int64, 0, len(requeue))
		for _, id := range requeue {
			if _, bad := unscorable[id]; !bad {
				covered = append(covered, id)
			}
		}
	}
	// Pairs are deleted for departed facts and for facts being scanned for the
	// first time — never for requeued partners, whose existing pairs are
	// exactly the asymmetric discoveries the requeue exists to preserve.
	//
	// "First time" is a no-op during ordinary operation (a new fact version has
	// new ids and therefore no pairs), and does the necessary cleanup in one
	// case: the session where the backfill finally completes. Nothing was
	// marked covered while the axis was partial, so that session rescans the
	// whole corpus — and its pairs should describe the finished axis, not the
	// union of every half-filled state it passed through on the way.
	return stats, d.Abstraction.ReplaceRestatementPairs(ctx, branch,
		append(dropped, added...), kept, covered)
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
func filterByBlendedCosine(ctx context.Context, d Deps, pairs []store.RestatementPair, dedupThreshold float64) ([]store.RestatementPair, map[int64]struct{}, error) {
	unscorable := map[int64]struct{}{}
	if len(pairs) == 0 {
		return nil, unscorable, nil
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
		return nil, unscorable, wrapf(reviewTool, err, "shortlist: body vectors")
	}

	out := make([]store.RestatementPair, 0, len(pairs))
	for _, p := range pairs {
		a, aok := vecs[p.AFactID]
		b, bok := vecs[p.BFactID]
		if !aok || !bok {
			// A fact with no stored vector cannot be scored — keeping the pair
			// would mean guessing whether dedup already caught it. Report the
			// missing side so the caller does not record it as covered.
			if !aok {
				unscorable[p.AFactID] = struct{}{}
			}
			if !bok {
				unscorable[p.BFactID] = struct{}{}
			}
			continue
		}
		if store.CosineSim(a, b) >= dedupThreshold {
			continue
		}
		out = append(out, p)
	}
	return out, unscorable, nil
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

// throttleWindow is how many shortlist verdicts the trailing resolution-rate is
// measured over; throttleMinVerdicts is how much evidence must accumulate
// before a corpus may defund itself; throttleProbeInterval is how many sessions
// a defunded corpus waits before spending ONE slot to test whether it is still
// right to be defunded. All three are PATIENCE BUDGETS, trading wasted judge
// slots against how fast a corpus can change its own mind.
//
// The probe is not a nicety. Without it the throttle is a LATCH: defunded means
// no emission, no emission means no verdicts, and no verdicts means the "a
// resolution restores it" clause can never fire. A corpus that briefly looked
// unproductive would be switched off permanently by evidence it is then
// forbidden from updating.
const (
	throttleWindow        = 10
	throttleMinVerdicts   = 5
	throttleProbeInterval = 5
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
	ResolutionRate float64
	ThrottleState  string
	// Probing is true when a defunded corpus spent its periodic probe slot —
	// the one path by which its own evidence can change.
	Probing bool
	// Failure names the step that failed, when one did. The shortlist degrades
	// to "no candidates" rather than failing a session, so without this line a
	// broken axis and a clean corpus look identical from outside.
	Failure string
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
	resolved := 0
	for _, v := range verdicts {
		if v.Resolved {
			resolved++
		}
	}
	rate := float64(resolved) / float64(len(verdicts))
	if resolved == 0 && len(verdicts) >= throttleMinVerdicts {
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
	h.ResolutionRate, h.ThrottleState = throttleState(verdicts)

	budget := shortlistBudget(n)
	probing := false
	if h.ThrottleState == throttleDefunded {
		// Defunded corpora still probe, or they could never recover.
		allowed, err := probeAllowed(ctx, d, branch)
		if err != nil {
			return nil, h, err
		}
		if !allowed {
			return nil, h, nil
		}
		probing = true
		budget = 1
	} else if err := d.Abstraction.SetProbeSessionsWaited(ctx, branch, 0); err != nil {
		// A funded corpus owes no probe; reset so a later defunding starts its
		// wait from now rather than inheriting an ancient count.
		return nil, h, wrapf(reviewTool, err, "shortlist: reset probe wait")
	}
	if budget == 0 {
		return nil, h, nil
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
		if !d.Scope.IsEmpty() && !pairTouchesScope(ctx, d, branch, p) {
			continue
		}
		out = append(out, p)
		if len(out) == budget {
			break
		}
	}
	if probing && len(out) > 0 {
		// The probe is spent only if it actually put something in front of the
		// judge. A session that found nothing to offer produced no evidence,
		// and charging it a probe would buy another full interval of silence
		// for nothing.
		h.Probing = true
		if err := d.Abstraction.SetProbeSessionsWaited(ctx, branch, 0); err != nil {
			return nil, h, wrapf(reviewTool, err, "shortlist: consume probe")
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
func planRestatementShortlist(ctx context.Context, d Deps, sess *store.PipelineSession, branch string, clusters [][]factForLLM) error {
	// health is emitted on EVERY path, including the failures below. The error
	// path is exactly where "the shortlist found nothing" has to stay
	// distinguishable from "the shortlist could not run" — a silent skip on a
	// corpus with 2,000 standing pairs reads as a clean bill of health.
	var health restatementHealth
	defer func() { recordRestatementHealth(sess, health) }()

	have, total, err := ensureTitleVectors(ctx, d, branch, titleBackfillBudget)
	if err != nil {
		health.Failure = "title backfill failed"
		log.Warn().Err(err).Str("session", sess.ID).
			Msg("review: title backfill failed; skipping restatement shortlist")
		return nil
	}
	if total == 0 {
		return nil // empty corpus: every ratio is zero and there is nothing to do
	}
	health.Coverage = float64(have) / float64(total)

	dedupThreshold := store.EmbedderThresholds(d.RI.Embedder()).Dedup
	refresh, err := refreshRestatementShortlist(ctx, d, branch, dedupThreshold)
	if err != nil {
		health.Failure = "shortlist refresh failed"
		log.Warn().Err(err).Str("session", sess.ID).
			Msg("review: restatement shortlist refresh failed; continuing without candidates")
		return nil
	}
	log.Debug().Str("session", sess.ID).
		Int("knn_queries", refresh.NeighbourQueries).
		Int("pairs_added", refresh.PairsAdded).
		Int("facts_requeued", refresh.FactsRequeued).
		Msg("review: restatement shortlist refreshed")

	// The budget scales with the CORPUS, not with this session's dirty seeds.
	// Seeds would make the feature a first-scan special case: every incremental
	// session sees a handful of changed facts, and a handful times 5-per-1000
	// is zero — so the shortlist would emit nothing for the entire life of a
	// repo after its first review, with a full pair cache sitting unread.
	pairs, selectHealth, err := selectRestatementCandidates(ctx, d, branch, clusters, total)
	if err != nil {
		health.Failure = "shortlist selection failed"
		log.Warn().Err(err).Str("session", sess.ID).
			Msg("review: restatement selection failed; continuing without candidates")
		return nil
	}
	selectHealth.Coverage = health.Coverage
	health = selectHealth

	if err := enqueueRestatementItems(ctx, d, sess, branch, pairs); err != nil {
		return err
	}
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
	if h.Failure != "" {
		return []string{
			fmt.Sprintf("restatement shortlist unavailable this session: %s "+
				"(no candidates — this is NOT a statement about the corpus)", h.Failure),
			fmt.Sprintf("abstraction coverage: %.0f%% of live epistemic facts", h.Coverage*100),
		}
	}
	return []string{
		fmt.Sprintf("abstraction coverage: %.0f%% of live epistemic facts", h.Coverage*100),
		fmt.Sprintf("standing restatement pairs: %d (title-cos p99 %.3f, p99.9 %.3f)",
			h.StandingPairs, h.TailP99, h.TailP999),
		fmt.Sprintf("operating point: title-cos %.3f (this corpus, this session)", h.OperatingPoint),
		fmt.Sprintf("restatement candidates emitted: %d", h.Emitted),
		fmt.Sprintf("shortlist throttle: %s%s (trailing resolution-rate %.0f%% over last %d judged)",
			h.ThrottleState, probeSuffix(h), h.ResolutionRate*100, throttleWindow),
	}
}

func probeSuffix(h restatementHealth) string {
	if h.Probing {
		return " (probing)"
	}
	return ""
}

// recordRestatementHealth hangs the session's health lines on the session
// object StartSession is holding.
//
// In memory, not in the session row: planning and responding happen inside the
// SAME StartSession call, on the same *PipelineSession pointer, so a database
// round trip would buy nothing. Health is read once, by the turn that produced
// it (invariants/synthesize/per-call-objects-no-session-state is about state
// that must SURVIVE a call — this deliberately does not).
// APPENDS rather than assigns (Phase 2, designer ruling 2026-08-21). From
// Phase 2 this field has more than one producer, and an assignment silently
// deletes whatever another mechanism already reported. Health is the only
// channel through which any of them says "I ran and found nothing", so losing
// a set of lines makes a broken subsystem indistinguishable from a clean
// corpus — the exact failure these descriptors exist to prevent.
//
// Ordering the callers correctly would also have worked, and did for one
// commit. It was rejected as the wrong fix: the ordering dependency is
// invisible at both call sites and a test can only guard the arrangement that
// exists today, so the trap survives its own fix. Appending removes the class.
//
// Called once per session, so this is behaviourally identical for the
// shortlist itself. TestMotifAliasHealth_CoexistsWithRestatementLines covers
// the interaction.
func recordRestatementHealth(sess *store.PipelineSession, h restatementHealth) {
	if sess == nil {
		return
	}
	sess.Health = append(sess.Health, healthLines(h)...)
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
	ids, err := d.Abstraction.FactIDsByPath(ctx, sess.Branch, paths)
	if err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: could not resolve shortlist verdict ids")
		return nil
	}
	return &judgedPair{
		APath: paths[0], BPath: paths[1],
		AFactID: ids[paths[0]], BFactID: ids[paths[1]],
	}
}

// recordShortlistVerdict records what the judge did with a shortlist pair, and
// retires the pair when the judge declined it.
//
// RESOLVED, not merged. A judge that consolidates a restatement by retracting
// the redundant half has done exactly the work this mechanism exists to buy, so
// counting only merges would defund a corpus that is consolidating
// successfully by another route — and a wrongly defunded corpus looks
// identical, from outside, to one where the shortlist genuinely finds nothing.
//
// A confidence "update" is deliberately NOT a resolution: it leaves both facts
// standing, so the redundancy this pair was offered for is still there.
func recordShortlistVerdict(ctx context.Context, d Deps, sess *store.PipelineSession, judged *judgedPair, res *PruneResult) {
	if judged == nil || res == nil {
		return
	}
	if judged.AFactID == 0 || judged.BFactID == 0 {
		// An endpoint could not be resolved to a live fact — it was merged or
		// retracted earlier in this same session, so this item was judging a
		// pair that no longer exists. Recording it would write a zero id into
		// the declined set (matching every other unresolved pair) and spend a
		// slot of the throttle window on a non-event.
		log.Debug().Str("session", sess.ID).
			Str("a", judged.APath).Str("b", judged.BPath).
			Msg("review: shortlist pair no longer live at judgement; not recording a verdict")
		return
	}

	resolved := false
	for _, m := range res.Merges {
		if pairCovered(m.Paths, judged.APath, judged.BPath) {
			resolved = true
			break
		}
	}
	for _, dec := range res.Decisions {
		if dec.Action == "retract" && (dec.Path == judged.APath || dec.Path == judged.BPath) {
			resolved = true
			break
		}
	}

	if err := d.Abstraction.RecordRestatementVerdict(ctx, sess.Branch, store.RestatementVerdict{
		APath: judged.APath, BPath: judged.BPath,
		AFactID: judged.AFactID, BFactID: judged.BFactID,
		Resolved: resolved, JudgedAt: time.Now().UTC(),
	}); err != nil {
		// Informational, like recordStats: the mutations are already committed,
		// and a lost verdict only makes the throttle slightly more optimistic.
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: could not record shortlist verdict")
		return
	}

	// Retire the pair either way. A resolved pair no longer exists as written;
	// a declined pair must not keep its place at the top of the ranking, or
	// after a few declining sessions the whole selection window would be
	// standing pairs the judge has already refused and nothing new could ever
	// be offered. The verdict log remains the record, and an edit to either
	// fact re-mints the pair through the ordinary KNN path — at which point it
	// is genuinely unseen, because a new version is a new id.
	if err := d.Abstraction.DeleteRestatementPair(ctx, sess.Branch, judged.AFactID, judged.BFactID); err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: could not retire judged shortlist pair")
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

// probeAllowed reports whether a DEFUNDED corpus may spend a probe slot this
// session: one every throttleProbeInterval sessions.
//
// This is the whole of the recovery path. A defunded corpus produces no
// verdicts, so its own evidence can never change without someone spending a
// slot to generate more — the probe spends exactly one, on a schedule, and the
// ordinary throttle takes over the moment a probe resolves.
//
// The waiting counter is advanced here but CONSUMED by the caller, and only
// when a pair was actually emitted: a probe that found nothing to offer has
// bought no information and must not cost an interval.
func probeAllowed(ctx context.Context, d Deps, branch string) (bool, error) {
	waited, err := d.Abstraction.ProbeSessionsWaited(ctx, branch)
	if err != nil {
		return false, wrapf(reviewTool, err, "shortlist: probe wait")
	}
	if waited+1 < throttleProbeInterval {
		if err := d.Abstraction.SetProbeSessionsWaited(ctx, branch, waited+1); err != nil {
			return false, wrapf(reviewTool, err, "shortlist: bump probe wait")
		}
		return false, nil
	}
	// At the interval: hold the counter here so every subsequent session is
	// also eligible until one of them actually emits.
	return true, nil
}
