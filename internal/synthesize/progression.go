package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// Prune-then-distill progression (knomit#135).
//
// THE DEFECT. Distill items were planned ALONGSIDE prune at session start, so
// every cluster was reasoned over twice in one session: once to decide whether
// its facts overlap, and again to synthesize upward from them as though they
// did not. When the prune verdict MERGED two facts, the distill item still held
// the pre-merge grouping — which is how synthesis fabricated corroboration out
// of one claim that had been recorded twice. Two facts saying the same thing
// look like two independent sources to anything that does not know they were
// just merged.
//
// THE FIX is an ordering, not a filter: every cluster goes to PRUNE first, and
// its distill item exists only if the prune verdict earns it. The prune verdict
// is a FREE CLASSIFIER, and that is the whole idea —
//
//   - acted on (merge / retract / update) → the facts overlapped. The cluster
//     STOPS this session. The action dirties the changed facts, so survivors
//     and merge outputs re-seed through the watermark and are re-clustered next
//     session, on settled material.
//   - all KEEP → the judge certified them distinct. That is synthesis's
//     legitimate input, and it is promoted IN THE SAME SESSION.
//
// SAME-SESSION IS LOAD-BEARING. "Plan distill next session for KEEPed clusters"
// silently never fires: an all-KEEP verdict changes nothing, so nothing is
// dirty, so the watermark never re-seeds those facts and the promotion is a
// promise the pipeline cannot keep.
//
// NO NEW MACHINERY. Enqueue-after-answer already exists and already ships:
// Strategy.Apply runs after the claim CAS is won, NextPipelineWorkItem filters
// on neither step type nor phase, and handlePhase advances only once the queue
// returns nil — so an item inserted here is served before the session
// completes. enqueueRaptorFollowups has been doing exactly this from the
// distill arm since RAPTOR landed. No dependency column, no schema change, no
// stored cursor.
//
// ORDERING IS FREE TOO. Prune items carry priority = cluster size (positive),
// distill items carry 0.0, and the queue is `priority DESC`. A promoted item
// therefore lands behind every prune item still queued, which is exactly the
// "every cluster goes to prune first" the ruling asks for — no sequencing code.

// promotedDistillKeyPrefix marks a distill item that a prune verdict promoted,
// so the queue census can tell a promoted item from a planned one.
const promotedDistillKeyPrefix = "distill-promoted-"

// pruneVerdictCertifiesDistinct reports whether a prune answer is the explicit,
// complete all-KEEP that earns promotion.
//
// FOUR CONDITIONS, and every one of them is load-bearing:
//
//  1. No merges. A merge is the judge saying these facts overlap.
//  2. At least one decision. `{"decisions":[],"merges":[]}` is a NON-ANSWER,
//     not an all-KEEP — promoting on it would manufacture synthesis work out of
//     silence, which is this area's standing rule (absence must be STATED,
//     never inferred) turned on its own promotion signal. It is also exactly
//     what the EffortNormal regression fixture answers prune with, so reading
//     empty as all-KEEP would make that fixture start producing distill items.
//  3. Every decision is `keep`. `retract` and `update` both ACT: each rewrites
//     or removes a fact, producing a new content-addressed row. The ruling
//     names merge/retract, and `update` is included here because the ruling's
//     own rationale — "the action dirties the changed facts, so they re-seed
//     via the watermark" — covers it identically. Distilling over a fact that
//     just changed underneath is the thing that rationale forbids.
//  4. The decisions COVER every fact in the item. validatePrunePaths accepts a
//     PARTIAL decision list — it checks that decisions reference known paths,
//     not that known paths have decisions — so "all the decisions were keep" is
//     satisfied by a judge that classified one fact out of five. Partial
//     coverage is silence for the rest, which is condition 2 again at a
//     different granularity.
func pruneVerdictCertifiesDistinct(result PruneResult, inputPaths []string) bool {
	if len(result.Merges) > 0 || len(result.Decisions) == 0 {
		return false
	}
	decided := make(map[string]bool, len(result.Decisions))
	for _, d := range result.Decisions {
		if d.Action != "keep" {
			return false
		}
		decided[d.Path] = true
	}
	for _, p := range inputPaths {
		if !decided[p] {
			return false
		}
	}
	return true
}

// promotesToDistill reports whether this prune item is the CLUSTER-shaped kind
// whose all-KEEP means "this group is worth synthesizing over".
//
// Shortlist items are excluded, and the distinction is a real one rather than
// bookkeeping. A `restate-N` item is a two-fact cross-cluster PAIR: its
// all-KEEP means "these two restatements are distinct", which is a judgement
// about redundancy between two specific facts and NOT the claim that some
// group is a coherent subject worth synthesizing upward from. Promoting one
// would hand distill a pair the clusterer never grouped.
func promotesToDistill(item *store.PipelineWorkItem) bool {
	return item != nil &&
		item.StepType == "prune" &&
		!strings.HasPrefix(item.ClusterKey, restatementClusterKeyPrefix)
}

// promoteClusterToDistill enqueues the distill item a certified-distinct prune
// cluster has earned, and reports what it did.
//
// THE PAYLOAD IS THE PRUNE ITEM'S OWN FACTS, and that is forced rather than
// chosen. The plan-time distill grouping is seeds-only, but the seed set is a
// plan-time value that no longer exists by the time Apply runs — and the only
// session-scoped persistence is pipeline_work_items, which has no spare column
// and no "blocked" flag (an unanswered row is servable by definition). So the
// seeds-only payload cannot be reconstructed here without the schema change
// this fix exists without. Two consequences, both deliberate and both reported
// in the health line rather than left for an auditor to discover:
//
//   - Promoted payloads are WHOLE-CLUSTER: they include neighbours the seed
//     scan never returned, and (post knomit#149) any cousin the cross-category
//     sweep attached. Plan-time distill groups were seeds-only.
//   - The population WIDENS slightly. A cluster with one seed and two pulled-in
//     neighbours is a prune item today but was never a distill group; it now
//     becomes one if the judge certifies it. This does not reintroduce the
//     redundancy #135 removes — that was distill firing on prune-ACTED,
//     overlapping clusters, which promotion-gating blocks outright.
//
// Every failure is logged and swallowed. The prune decisions are already
// committed by the time this runs, so returning an error here would report a
// failure for work that succeeded; losing a promotion costs one round of
// synthesis, not correctness. That is enqueueRaptorFollowups' own contract, for
// the same reason.
func promoteClusterToDistill(
	ctx context.Context,
	d Deps,
	sess *store.PipelineSession,
	item *store.PipelineWorkItem,
) {
	var facts []factForLLM
	if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Str("cluster", item.ClusterKey).
			Msg("review: certified-distinct cluster could not be decoded for promotion")
		return
	}
	// One fact is not a group — there is no pattern to find across a single
	// fact, which is the same floor distillGroups applies at plan time.
	if len(facts) < 2 {
		return
	}

	enqueued := 0
	for ci, chunk := range chunkFacts(facts, maxItemBytes) {
		payload, err := json.Marshal(chunk)
		if err != nil {
			log.Warn().Err(err).Str("session", sess.ID).Str("cluster", item.ClusterKey).
				Msg("review: promoted distill chunk could not be marshalled")
			continue
		}
		// Depth 0: this is a FIRST-round distill over corpus facts, not a
		// RAPTOR follow-up over synthesis output. Handing it a deeper depth
		// would spend the recursion budget the follow-ups need.
		//
		// Priority 0.0 matches every other depth-0 distill item, which is what
		// puts it behind the prune items still queued.
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "distill",
			ClusterKey: fmt.Sprintf("%s%s-%d", promotedDistillKeyPrefix, item.ClusterKey, ci),
			FactsJSON:  string(payload),
			Priority:   0.0,
		}); err != nil {
			log.Warn().Err(err).Str("session", sess.ID).Str("cluster", item.ClusterKey).
				Msg("review: promoted distill item could not be enqueued")
			continue
		}
		enqueued++
	}
	if enqueued == 0 {
		return
	}
	sess.Health = append(sess.Health, fmt.Sprintf(
		"prune→distill: %s came back all-KEEP, so the judge certified its %d facts "+
			"distinct and %d distill item(s) were promoted for THIS session. The "+
			"promoted payload is the WHOLE CLUSTER — it carries neighbours the seed "+
			"scan did not return, and any cross-category cousin attached to it, "+
			"which plan-time distill groups did not.",
		item.ClusterKey, len(facts), enqueued))
}

// recordProgressionStop states that a cluster was NOT promoted, and why.
//
// Absence of work must be STATED, never inferred — the rule this area keeps
// re-learning. Without this line a cluster that stopped because its judge acted
// on it is indistinguishable from a cluster the progression forgot, and the
// whole point of #135 is that distill NOT running is now a designed outcome
// rather than an omission.
func recordProgressionStop(sess *store.PipelineSession, item *store.PipelineWorkItem, stats *ReviewStats) {
	if sess == nil || item == nil {
		return
	}
	if stats != nil && (stats.Merged > 0 || stats.Pruned > 0 || stats.Updated > 0) {
		sess.Health = append(sess.Health, fmt.Sprintf(
			"prune→distill: %s was ACTED on (%d merged, %d retracted, %d updated), so it "+
				"stops here this session. The changed facts are now dirty and re-seed "+
				"through the watermark, so this cluster is re-clustered and considered "+
				"for synthesis NEXT session, on settled material.",
			item.ClusterKey, stats.Merged, stats.Pruned, stats.Updated))
		return
	}
	sess.Health = append(sess.Health, fmt.Sprintf(
		"prune→distill: %s was not promoted. The verdict was neither a complete "+
			"all-KEEP nor an action — an empty or PARTIAL decision list leaves part of "+
			"the cluster unclassified, and synthesis is not planned on silence.",
		item.ClusterKey))
}
