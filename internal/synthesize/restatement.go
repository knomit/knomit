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
	StructuralAdded  int  // pairs found by path identity or a rare identifier
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
func refreshRestatementShortlist(ctx context.Context, d Deps, branch string) (refreshStats, error) {
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

	// The structural pass (#127). The KNN above finds what is CLOSE; this finds
	// what the corpus's own filing and vocabulary say is the SAME — pairs no
	// vector neighbourhood contains, which is precisely the population that
	// survives longest.
	//
	// Degrades to "no structural candidates" rather than failing the refresh:
	// it is an addition to a mechanism that worked without it.
	structural, serr := structuralPairs(ctx, d, branch, requeue, declined)
	if serr != nil {
		log.Warn().Err(serr).Msg("review: structural duplicate detection skipped this session")
	} else {
		candidates = append(candidates, structural...)
		stats.StructuralAdded = len(structural)
	}

	stats.PairsAdded = len(candidates)

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
	// Coverage is a statement about the TITLE axis and nothing else, because
	// the title KNN above is the entire scan. An earlier version also withheld
	// coverage from facts whose blended vector could not be read — necessary
	// while a blended-cosine filter ran here, meaningless now that none does.
	var covered []int64
	if complete {
		covered = append([]int64(nil), requeue...)
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
		append(dropped, added...), candidates, covered)
}

// newRestatementPair canonicalises a pair so A-B and B-A are one row.
func newRestatementPair(id int64, path string, n store.TitleNeighbour) store.RestatementPair {
	p := store.RestatementPair{
		APath: path, BPath: n.Path,
		AFactID: id, BFactID: n.FactID,
		TitleCos:  n.Similarity,
		MatchKind: store.MatchTitleKNN,
	}
	if p.BPath < p.APath {
		p.APath, p.BPath = p.BPath, p.APath
		p.AFactID, p.BFactID = p.BFactID, p.AFactID
	}
	return p
}

// NOTE ON THE FILTER THAT USED TO LIVE HERE (#127).
//
// This function once dropped every candidate pair whose stored blended vectors
// sat at or above the model's calibrated dedup threshold, on the rationale that
// "mergeFacts already merges them mechanically, so spending a judge slot on one
// is pure waste."
//
// That rationale is false for precisely the population this shortlist exists to
// serve. dedupCluster's mechanical merge only ever pairs facts that are in the
// SAME cluster — every search hit is gated on cluster membership before it can
// become a mergePair — and the shortlist exists because restatements whose
// halves cluster APART are judged by nothing (gotchas/synthesize/prune-scope).
// So a cross-cluster pair above the floor was deleted here as already-handled
// and then handled by nothing: being a CERTAIN duplicate was the disqualifier.
//
// Measured on the live core corpus before the removal: all six confirmed
// duplicate pairs sat at blended cosine 0.83–0.97 against a floor of 0.82 and
// were absent from a 14,768-row standing cache whose surviving pairs topped out
// at 0.77. The judge was being shown everything except the duplicates.
//
// The exclusion it was trying to express — "prune already sees this pair" — is
// real, and is implemented once and correctly at SELECTION time as a cluster
// co-membership check (clusterCoMembership). That check is exact and
// session-aware; the cosine version was a proxy for it, and the proxy was
// inverted.
//
// Nothing replaces it: an above-floor pair is the shortlist's best candidate,
// not its waste. It is also the pair a mechanical merge would handle WORST —
// mergeFacts picks a winner and discards the loser's body, while the judge is
// required to preserve both.

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

// shortlistMotifWiden is the §6 motif signal: how much further down THIS
// repo's own title-cosine ranking a pair is considered when its two facts
// share an exact canonical motif.
//
// An ELIGIBILITY WIDENER, not a score bonus (designer ruling Q2). A bonus
// added to title_cos would be a corpus-property constant wearing a different
// hat — it would claim to know how much a shared motif is worth in cosine
// units, on every corpus. This claims only that a shared motif is worth
// LOOKING further, and how far "further" reaches is still decided entirely by
// the repo's own distribution: the band is a rank cut, so on a corpus with a
// tight distribution it reaches a shorter absolute distance than on a loose
// one.
//
// The judge-slot budget is unchanged. Widening changes what may be considered,
// never how much is spent — a motif-rich corpus gets better candidates for the
// same money, not more of them.
//
// A SELECTION-POLICY constant, the same class as judgePairPermille (MN13).
const shortlistMotifWiden = 3

// structuralAllowance is how many judge slots ONE SESSION may spend on the
// structural detection route, and it is spent WHATEVER THE THROTTLE SAYS.
//
// MN13 classification: a RESOURCE BUDGET, the same class as shortlistOverfetch
// and pairNeighbourK — how much one session is willing to spend on this route.
// It is NOT a claim about how many structural duplicates a corpus contains, and
// nothing here is gated on a "≥N standing" floor, which would be exactly that
// claim.
//
// WHY IT IS NOT THE THROTTLE'S TO WITHHOLD (knomit#155). The structural route
// used to sit behind the ordinary band's `budget >= 2`, which made it
// unreachable in the one state it was built for: a defunded corpus probes with
// budget 1, one below the gate, so a corpus whose judge had resolved nothing
// could never be shown the evidence that might change its mind. That is a
// self-defunding latch — the penalty blocking its own recovery — and the fix is
// to stop expressing this route's cost in the ordinary band's currency at all.
// The throttle governs how much a corpus spends on TITLE-RANKED candidates,
// which is what its verdict history is evidence about; it has never judged a
// structural pair, so it has no evidence to withhold one on.
const structuralAllowance = 5

// Throttle states, reported in health output.
//
// RESOLVED, not merged: a verdict counts when the judge merged the pair OR
// retracted its redundant half (the verdicts-are-resolved-not-merged ruling).
// These comments used to say "merged". They predate that ruling and named a
// NARROWER event than the code has ever counted, which is half of why the
// printed state looked impossible on live corpora (knomit#117b) — the other
// half is the fall-through that throttleState now closes.
const (
	throttleOptimistic = "optimistic" // no verdicts yet — the cap bounds the downside
	throttleFunded     = "funded"     // the judge resolved a pair recently
	throttleUnproven   = "unproven"   // judged, none resolved, too little evidence to defund
	throttleDefunded   = "defunded"   // enough judged, none resolved: stop spending
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
	Dropped        int
	ResolutionRate float64
	ThrottleState  string
	// Judged is how many verdicts the rate above was actually computed over.
	// The health line used to print the throttleWindow CONSTANT instead, so a
	// corpus with no verdicts at all still read "over last 10 judged"
	// (knomit#117b). The denominator has to be the real one or the rate above
	// it cannot be interpreted.
	Judged int
	// MotifWidened counts pairs admitted ONLY because their facts share a
	// canonical motif — candidates the title axis alone would not have reached.
	// Reported so the signal's contribution is visible rather than inferred.
	MotifWidened int
	// MotifSlotUsed is true when the reserved slot DISPLACED an ordinary
	// candidate — i.e. the ordinary band could have filled the budget and a
	// widened pair took a slot anyway. That is the case where the signal cost
	// something, and the one worth reporting: a widened pair admitted into a
	// slot nothing else wanted is free.
	MotifSlotUsed bool
	// StructuralAvailable / StructuralOffered describe the #127 detection
	// route: how many structurally matched pairs were eligible this session,
	// and how many actually reached the judge. Observability only — a
	// detection that is never offered has to be visible as such, which is the
	// failure this route exists to end.
	StructuralAvailable int
	StructuralOffered   int
	// StandingStructural is how many structurally matched pairs the cache
	// holds at all, whatever this session's scope and clusters made eligible.
	StandingStructural int
	// StructuralOutOfScope counts structural pairs offered to a SCOPED session
	// whose halves both sit outside that scope. The structural route ignores
	// the scope filter by design, which is a widening — and a widening the
	// operator is not told about is the defect the #122 family exists to end.
	StructuralOutOfScope int
	// SweepOrderStable reports whether the sweep's oldest-first order currently
	// means what it says. It is true only once the title axis is COMPLETE:
	// while the axis is filling, every pair is rescanned and re-minted each
	// session, which reassigns every rowid (see
	// RestatementPairsByMatchKindOldest). Reported rather than assumed —
	// a degenerated sweep order looks exactly like a working one from outside.
	SweepOrderStable bool
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
// verdicts, unproven once pairs have been judged with nothing resolved but too
// few to conclude, defunded once enough have been judged with none resolved,
// funded the moment one resolves.
//
// unproven is not a cosmetic split of funded (knomit#117b). Every
// 0 < len(verdicts) < throttleMinVerdicts with nothing resolved used to fall
// THROUGH to funded, so the line claimed "the judge resolved a pair recently"
// on the exact evidence that says it did not — observed live on knomit-kb at
// one judged KEEP. That interval is also the trajectory INTO defunding, which
// is the thing an operator needs to see coming rather than discover after the
// corpus has gone quiet.
//
// Behaviourally inert BY CONSTRUCTION, and that is load-bearing: only
// throttleDefunded is read by any branch, so an unproven corpus budgets and
// probes exactly as a funded or optimistic one does. This changes what is
// PRINTED, never what is spent.
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
	if resolved == 0 {
		if len(verdicts) >= throttleMinVerdicts {
			return rate, throttleDefunded
		}
		return rate, throttleUnproven
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
	h.StandingStructural = stats.Structural

	verdicts, err := d.Abstraction.RecentRestatementVerdicts(ctx, branch, throttleWindow)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: verdicts")
	}
	h.ResolutionRate, h.ThrottleState = throttleState(verdicts)
	h.Judged = len(verdicts)

	// The structural sweep is decided BEFORE the throttle and independently of
	// it (knomit#155). Ordering matters: every early return below is a
	// throttle decision about the ordinary band, and the whole point of the
	// dedicated allowance is that those decisions do not reach this route.
	coGrouped := clusterCoMembership(clusters)
	structural, err := selectStructuralSweep(ctx, d, branch, coGrouped)
	if err != nil {
		return nil, h, err
	}
	h.StructuralAvailable = len(structural)
	// Whether "oldest-first" currently means what it says — see
	// RestatementPairsByMatchKindOldest. Reported rather than assumed, because
	// a sweep whose order has silently degenerated looks exactly like one that
	// has not.
	h.SweepOrderStable = h.Coverage >= 1
	if !d.Scope.IsEmpty() {
		// The structural route deliberately ignores the scope filter: a
		// cross-category structural pair is corpus-wide evidence by
		// construction. That is a SCOPE WIDENING, and a widening nobody is
		// told about is the #122 family's own defect. Count them so the health
		// line can say so.
		for _, p := range structural {
			if !pairTouchesScope(ctx, d, branch, p) {
				h.StructuralOutOfScope++
			}
		}
	}

	budget := shortlistBudget(n)
	probing := false
	throttleClosed := false
	if h.ThrottleState == throttleDefunded {
		// Defunded corpora still probe, or they could never recover.
		allowed, err := probeAllowed(ctx, d, branch)
		if err != nil {
			return nil, h, err
		}
		if !allowed {
			throttleClosed = true
		} else {
			probing = true
			budget = 1
		}
	} else if err := d.Abstraction.SetProbeSessionsWaited(ctx, branch, 0); err != nil {
		// A funded corpus owes no probe; reset so a later defunding starts its
		// wait from now rather than inheriting an ancient count.
		return nil, h, wrapf(reviewTool, err, "shortlist: reset probe wait")
	}
	if budget == 0 {
		throttleClosed = true
	}
	if throttleClosed {
		// The ordinary band is shut. The structural allowance is not — this is
		// the latch break, and it is the only path by which a defunded corpus
		// is shown evidence its own verdict history says nothing about.
		return emitStructuralOnly(structural, &h), h, nil
	}

	// Over-fetch: the exclusions below are decided per candidate, and cutting to
	// the budget first would let one excluded pair silently shrink a batch the
	// throttle had funded.
	raw, err := d.Abstraction.RestatementPairsByRank(ctx, branch, budget*shortlistOverfetch)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: rank")
	}

	// The motif widener (§6): pairs sharing an exact canonical motif are
	// considered further down this repo's own ranking than pairs that do not.
	// Fetched as one wider read and split below, so the ordinary band is
	// exactly what it was before motifs existed.
	widened, err := d.Abstraction.RestatementPairsByRank(ctx, branch, budget*shortlistOverfetch*shortlistMotifWiden)
	if err != nil {
		return nil, h, wrapf(reviewTool, err, "shortlist: widened rank")
	}
	ordinaryDepth := len(raw)

	// Classify first, select second. An earlier version selected in one pass
	// and stopped at the budget, which meant a widened pair — below the
	// ordinary band by construction — was only ever reached when the ordinary
	// band underfilled. That made the signal contribute nothing in exactly the
	// case it exists for: pairs the title axis UNDER-RANKS (designer ruling
	// Q10).
	eligible := func(p store.RestatementPair) bool {
		if _, ok := coGrouped[pathPairKey(p.APath, p.BPath)]; ok {
			return false
		}
		if !d.Scope.IsEmpty() && !pairTouchesScope(ctx, d, branch, p) {
			return false
		}
		return true
	}

	var ordinary, motifPairs []store.RestatementPair
	for i, p := range widened {
		if !eligible(p) {
			continue
		}
		if i < ordinaryDepth {
			ordinary = append(ordinary, p)
			continue
		}
		// Past the ordinary band. Only a shared canonical motif buys a look
		// this far down — and only an EXACT one: the loose tiers are for a
		// reader who judges what comes back, never for something that spends a
		// judge slot (§6).
		shared, serr := pairSharesCanonicalMotif(ctx, d, branch, p)
		if serr == nil && shared {
			motifPairs = append(motifPairs, p)
		}
	}

	// RESERVE one slot for a widened pair when one exists. A shared canonical
	// motif is evidence ORTHOGONAL to title similarity — evidence the title
	// axis cannot see — so the pairs it identifies are below the title-ranked
	// band by definition, and a widener that fires only on underfill is
	// decorative. The reservation is a BUDGET ALLOCATION, not a threshold
	// (MN13), and it is the probe pattern's shape: a bounded slot spent for
	// information the main ranking cannot produce. A bad widened pair costs one
	// judgment, and the kept-pair exclusion retires it.
	//
	// Never at budget 1. There the reserved slot would BE the whole budget, and
	// a corpus that can afford one judgment should spend it on its
	// best-evidenced candidate rather than on orthogonal evidence about a
	// lower-ranked one.
	//
	// The structural route USED TO SHARE this arithmetic, as a second
	// one-slot reservation. It no longer does: it has its own allowance, spent
	// outside the budget entirely (knomit#155). So the only thing left to
	// arbitrate here is the motif slot against the ordinary band.
	motifReserved := 0
	if budget >= 2 && len(motifPairs) > 0 {
		motifReserved = 1
	}

	var out []store.RestatementPair
	taken := map[string]struct{}{}
	add := func(p store.RestatementPair) bool {
		key := pathPairKey(p.APath, p.BPath)
		if _, dup := taken[key]; dup {
			return false
		}
		taken[key] = struct{}{}
		out = append(out, p)
		return true
	}

	// Structural first, on its OWN allowance — it is not competing for the
	// budget any more, so nothing it takes is taken from the ordinary band.
	// It goes first only so the ordinary band's cosine stays the LAST thing
	// added, which is what the operating point below reads.
	for _, p := range structural {
		if add(p) {
			h.StructuralOffered++
		}
	}
	// The budget-funded bands. `budgeted` counts only what the BUDGET paid
	// for: `len(out)` would now include the structural allowance and would
	// shrink the ordinary band by however many structural pairs happened to
	// stand — the allowance silently becoming a tax on the very band it was
	// separated from.
	budgeted := 0
	for _, p := range ordinary {
		if budgeted >= budget-motifReserved {
			break
		}
		if add(p) {
			budgeted++
		}
	}
	for _, p := range motifPairs {
		if budgeted >= budget {
			break
		}
		if add(p) {
			budgeted++
			h.MotifWidened++
		}
	}
	// A reserved slot the ordinary band could not have used is not "reserved"
	// in any meaningful sense — report only when the signal actually displaced
	// something (designer rider).
	h.MotifSlotUsed = h.MotifWidened > 0 && len(ordinary) >= budget

	// Selection can only PROPOSE a probe; whether the slot is spent depends on
	// what reached the judge, and only enqueue knows that. So the consumption
	// moved to planRestatementShortlist (knomit#117b).
	//
	// probeAllowed has always documented the contract as "consumed by the
	// caller, and only when a pair was actually emitted" — this is the first
	// code that meets it. The gap mattered because selected != served since
	// knomit#117a: a probe whose one pair fails to load at item creation put
	// NOTHING in front of the judge, yet bought a full interval of silence.
	// That is the self-defunding latch re-introduced at a slower rate.
	h.Probing = probing && budgeted > 0
	if budgeted > 0 {
		// The operating point is not a threshold anyone chose: it is whatever
		// absolute cosine the last funded pair happens to sit at in THIS repo.
		// Reported because it is a corpus fingerprint — the same code on
		// another corpus prints a different number.
		//
		// FUNDED is the operative word, and it is why this reads `budgeted`
		// rather than `len(out)`. The structural allowance is not funded by the
		// budget, so its cosine is not this corpus's operating point — and on a
		// defunded corpus, where the allowance is the ONLY thing emitted, using
		// the last element would print a structural cosine as though the
		// throttle had chosen it.
		h.OperatingPoint = out[len(out)-1].TitleCos
	}
	h.Emitted = len(out)
	return out, h, nil
}

// selectStructuralSweep is the structural route's whole selection: the oldest
// standing path-identity pairs this session's clusters do not already hold.
//
// PATH-IDENTITY ONLY, for now. The rare-token route is the wider, weaker net
// (store.MatchRareToken), and its merge rate is unmeasured because the route it
// shares was inert — so it stays closed until the path-identity sample says
// what a structural offer is actually worth. Opening both at once would spend
// the sample on a population that cannot be told apart afterwards.
//
// The SCOPE filter is deliberately absent. `eligible` applies it to the
// title-ranked band, where it belongs — a session asked to work on one area
// should not spend its funded slots elsewhere. A structural pair is not that:
// its evidence is the corpus's own filing, which is corpus-wide by
// construction, and there is no scoped view that would ever co-present the two
// halves. The widening is real, so it is COUNTED and reported rather than done
// quietly (h.StructuralOutOfScope).
func selectStructuralSweep(ctx context.Context, d Deps, branch string, coGrouped map[string]struct{}) ([]store.RestatementPair, error) {
	// Over-fetch for the same reason the ordinary band does: the exclusion
	// below is decided per candidate, so cutting to the allowance first would
	// let one co-clustered pair silently shrink the sweep.
	sp, err := d.Abstraction.RestatementPairsByMatchKindOldest(ctx, branch,
		[]string{store.MatchPathIdentity}, structuralAllowance*shortlistOverfetch)
	if err != nil {
		return nil, wrapf(reviewTool, err, "shortlist: structural sweep")
	}
	var out []store.RestatementPair
	for _, p := range sp {
		if len(out) >= structuralAllowance {
			break
		}
		// The one exclusion that survives on this route: prune already sees
		// this pair in one cluster, so a shortlist slot would buy a second
		// judgement of the same question.
		if _, ok := coGrouped[pathPairKey(p.APath, p.BPath)]; ok {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// emitStructuralOnly is the return the throttle-closed paths take.
//
// A defunded corpus that is not probing, and a corpus whose budget rounds to
// zero, both used to return NOTHING here. That is what made the structural
// detection inert exactly where it mattered: the corpus with the least evidence
// that consolidation is worth anything is the corpus most in need of being
// shown a near-certain duplicate. The allowance is spent; the budget is not.
func emitStructuralOnly(structural []store.RestatementPair, h *restatementHealth) []store.RestatementPair {
	h.StructuralOffered = len(structural)
	h.Emitted = len(structural)
	return structural
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
//
// Returns what actually reached the judge, and what did not (knomit#117a). The
// count and the insert were separated by a `continue`, so a candidate whose
// pair could not be loaded evaporated between them: no item, no health line, no
// warning. Measured on core as `restatement candidates emitted: 8` beside ZERO
// restate- items in a fully-drained 48-item queue. The number was not wrong
// about what it counted — it counted SELECTION and was read as SERVICE, and
// nothing in the output could tell the two apart.
func enqueueRestatementItems(ctx context.Context, d Deps, sess *store.PipelineSession, branch string, pairs []store.RestatementPair) (served, dropped int, err error) {
	for i, p := range pairs {
		facts := make([]factForLLM, 0, 2)
		for _, path := range []string{p.APath, p.BPath} {
			f, err := d.Search.GetByPath(ctx, branch, path)
			if err != nil {
				return served, dropped, wrapf(reviewTool, err, "shortlist: read %s", path)
			}
			if f == nil {
				// Raced a retraction, or the pair names a path that no longer
				// resolves — drop rather than half-ship. Dropping is still
				// right; doing it silently was not.
				break
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
			dropped++
			// WARN, not Debug: this is a computed candidate the corpus paid a
			// KNN query for, and it is being discarded. The pair is NOT
			// retired — no verdict is recorded — so it stays standing and can
			// be re-selected once its half resolves again. That makes a
			// persistent drop a LOOP rather than a loss, which is worth
			// seeing in a log.
			log.Warn().Str("session", sess.ID).Str("a", p.APath).Str("b", p.BPath).
				Msg("review: restatement candidate dropped — a half did not resolve")
			continue
		}
		factsJSON, err := json.Marshal(facts)
		if err != nil {
			return served, dropped, wrapf(reviewTool, err, "shortlist: marshal pair %d", i)
		}
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "prune",
			ClusterKey: fmt.Sprintf("%s%d", restatementClusterKeyPrefix, i),
			FactsJSON:  string(factsJSON),
			Priority:   restatementPriority,
		}); err != nil {
			return served, dropped, wrapf(reviewTool, err, "shortlist: insert item %d", i)
		}
		served++
	}
	return served, dropped, nil
}

// applyEmissionOutcome corrects the health block from what SELECTION chose to
// what enqueue actually served (knomit#117a).
//
// It exists as its own function because the two numbers are produced in
// different places — selectRestatementCandidates sets Emitted, and the drop
// happens later, at enqueue — so without an explicit hand-off the health line
// keeps reporting selection while the queue reflects service. That gap is the
// whole bug, and a fix that only changed enqueue's return value would leave the
// number an operator READS exactly as wrong as before.
func applyEmissionOutcome(h *restatementHealth, served, dropped int) {
	h.Emitted = served
	h.Dropped = dropped
	// The same correction applied to the probe (knomit#117b). Selection
	// proposed one; a slot is only SPENT if something actually reached the
	// judge. Downgrading here also stops the health line rendering "(probing)"
	// for a session that probed nothing — the identical selected-vs-served
	// lie this function exists to end, wearing the throttle's clothes.
	h.Probing = h.Probing && served > 0
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

	refresh, err := refreshRestatementShortlist(ctx, d, branch)
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

	served, dropped, err := enqueueRestatementItems(ctx, d, sess, branch, pairs)
	// Reconcile on BOTH paths, before the early return (PR #130, LOW-1).
	// Emitted was recorded by selection; only enqueue knows what survived to
	// the queue. An enqueue that fails PART WAY through has already inserted
	// some items, and returning without reconciling leaves the deferred
	// recordRestatementHealth reporting selection's count over a partial
	// queue — this issue's own bug, re-opened on the error path.
	//
	// Unobservable today (StartSession discards the result on error), which is
	// exactly why it is worth closing now rather than when something starts
	// reading it.
	applyEmissionOutcome(&health, served, dropped)
	if health.Probing {
		// Consume the probe HERE, and only here: health.Probing is true at this
		// point only if a pair actually reached the judge, which is the
		// contract probeAllowed documents.
		//
		// A failure is not fatal and is self-healing: probeAllowed HOLDS the
		// counter at the interval rather than bumping past it, so a corpus that
		// could not record the consumption stays eligible and probes again next
		// session. Failing a review over that bookkeeping write would cost more
		// than the one extra probe it prevents.
		if perr := d.Abstraction.SetProbeSessionsWaited(ctx, branch, 0); perr != nil {
			log.Warn().Err(perr).Str("session", sess.ID).
				Msg("review: probe slot not consumed; corpus stays eligible next session")
		}
	}
	if err != nil {
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
		emittedLine(h),
		fmt.Sprintf("shortlist throttle: %s%s (trailing resolution-rate %.0f%% over last %d judged)",
			h.ThrottleState, probeSuffix(h), h.ResolutionRate*100, h.Judged),
		// The motif signal's actual contribution, not its existence (designer
		// rider Q10). The GATE package needs to state how often it FIRED, and
		// a line that only ever said "enabled" could not support that claim.
		motifSignalLine(h),
		// Likewise for the structural route (#127). Detection that never
		// reaches the judge is this issue's own failure mode, so the line
		// reports what was OFFERED beside what was found — the two numbers
		// diverging is the symptom, and it has to be visible.
		structuralSignalLine(h),
	}
}

// emittedLine reports what actually reached the judge, and names any candidate
// that did not (knomit#117a).
//
// The drop clause appears ONLY when something dropped. A block that mentions
// drops every session trains a reader to skip the line, which is how the
// original silence would come back wearing a number.
func emittedLine(h restatementHealth) string {
	if h.Dropped == 0 {
		return fmt.Sprintf("restatement candidates emitted: %d", h.Emitted)
	}
	return fmt.Sprintf("restatement candidates emitted: %d (%d selected but dropped "+
		"unserved — a half no longer resolves; the pair stays standing and will "+
		"be re-offered)", h.Emitted, h.Dropped)
}

// structuralSignalLine reports the path-identity / rare-token route.
//
// Standing, eligible and offered are three different populations and the gaps
// between them are the interesting part: pairs detected but ineligible are
// pairs prune already sees, while pairs eligible but not offered are the
// budget saying no.
func structuralSignalLine(h restatementHealth) string {
	if h.StandingStructural == 0 {
		return "structural duplicate detection: no path-identity or rare-token pairs stand in this corpus"
	}
	line := fmt.Sprintf(
		"structural duplicate detection: %d standing, %d eligible this session, %d offered "+
			"(allowance %d/session, path-identity only — rare-token stays closed until the "+
			"path-identity merge rate justifies it)",
		h.StandingStructural, h.StructuralAvailable, h.StructuralOffered, structuralAllowance)
	// Say which order the sweep is actually in. "Oldest-first" is exact only on
	// a complete axis; while the axis fills, every pair is re-minted each
	// session and the order is deterministic but not age-stable. A sweep that
	// has silently degenerated reads identically to one that has not, so the
	// regime is stated rather than implied (see
	// RestatementPairsByMatchKindOldest).
	if h.SweepOrderStable {
		line += ". Sweep order: oldest-first (axis complete)"
	} else {
		line += ". Sweep order: NOT yet age-stable — the title axis is still filling, so " +
			"pairs are re-minted each session and mint order churns"
	}
	if h.StructuralOutOfScope > 0 {
		line += fmt.Sprintf(". %d structural pair(s) offered from OUTSIDE this session's scope "+
			"— structural evidence is corpus-wide by construction, so this route ignores the "+
			"scope filter on purpose", h.StructuralOutOfScope)
	}
	return line
}

// motifSignalLine reports what the §7 motif widener contributed this session.
func motifSignalLine(h restatementHealth) string {
	switch {
	case h.MotifWidened == 0:
		return "motif signal: no pair admitted by shared motif this session"
	case h.MotifSlotUsed:
		return fmt.Sprintf("motif signal: %d pair(s) admitted by shared motif, "+
			"using the reserved slot (displaced an ordinary candidate)", h.MotifWidened)
	default:
		return fmt.Sprintf("motif signal: %d pair(s) admitted by shared motif, "+
			"into slots the ordinary band did not fill", h.MotifWidened)
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

// pairSharesCanonicalMotif reports whether both facts in a pair carry a motif
// resolving to the same canonical cluster.
//
// EXACT tier only. The loose tiers exist for a reader who judges what comes
// back; this decides whether to spend a judge slot, which §6 puts squarely on
// the automation side of that line.
//
// Degrades to false on any error: the widener is an ADDITION to the shortlist,
// and a corpus whose vocabulary cannot be read should still get its ordinary
// candidates rather than none.
func pairSharesCanonicalMotif(ctx context.Context, d Deps, branch string, p store.RestatementPair) (bool, error) {
	if d.Motifs == nil {
		return false, nil
	}
	a, err := d.Search.GetByPath(ctx, branch, p.APath)
	if err != nil || a == nil || len(a.Motifs) == 0 {
		return false, nil
	}
	b, err := d.Search.GetByPath(ctx, branch, p.BPath)
	if err != nil || b == nil || len(b.Motifs) == 0 {
		return false, nil
	}
	seen := make(map[string]struct{}, len(a.Motifs))
	for _, m := range a.Motifs {
		canonical, cerr := d.Motifs.CanonicalID(ctx, branch, m)
		if cerr != nil {
			continue
		}
		seen[canonical] = struct{}{}
	}
	for _, m := range b.Motifs {
		canonical, cerr := d.Motifs.CanonicalID(ctx, branch, m)
		if cerr != nil {
			continue
		}
		if _, ok := seen[canonical]; ok {
			return true, nil
		}
	}
	return false, nil
}

// ── structural detection ──────────────────────────────────────────────────

// structuralPairs finds duplicate candidates by path identity and by shared
// rare identifier tokens, and scores them on the title axis so they rank
// sensibly against each other.
//
// A pair whose two title vectors are not both stored is DROPPED rather than
// given an invented score: the ranking column means "title cosine", and a
// placeholder there would be a number no measurement produced. The pair is not
// lost — the axis backfill fills in over sessions, and the next refresh that
// rescans either fact mints it.
func structuralPairs(ctx context.Context, d Deps, branch string, requeue []int64, declined map[string]struct{}) ([]store.RestatementPair, error) {
	if len(requeue) == 0 {
		return nil, nil
	}
	titles, err := d.Abstraction.LiveEpistemicFactTitles(ctx, branch)
	if err != nil {
		return nil, wrapf(reviewTool, err, "shortlist: live fact titles")
	}
	matches := buildIdentityIndex(titles).structuralCandidates(requeue)
	if len(matches) == 0 {
		return nil, nil
	}

	idSet := map[int64]struct{}{}
	for _, m := range matches {
		idSet[m.a] = struct{}{}
		idSet[m.b] = struct{}{}
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	vecs, err := d.Abstraction.TitleVectorsByFactID(ctx, ids)
	if err != nil {
		return nil, wrapf(reviewTool, err, "shortlist: title vectors for structural pairs")
	}

	out := make([]store.RestatementPair, 0, len(matches))
	for _, m := range matches {
		if _, ok := declined[store.FactIDPairKey(m.a, m.b)]; ok {
			continue
		}
		va, aok := vecs[m.a]
		vb, bok := vecs[m.b]
		if !aok || !bok {
			continue
		}
		p := store.RestatementPair{
			APath: titles[m.a].Path, BPath: titles[m.b].Path,
			AFactID: m.a, BFactID: m.b,
			TitleCos:  store.CosineSim(va, vb),
			MatchKind: m.kind,
		}
		if p.BPath < p.APath {
			p.APath, p.BPath = p.BPath, p.APath
			p.AFactID, p.BFactID = p.BFactID, p.AFactID
		}
		out = append(out, p)
	}
	return out, nil
}
