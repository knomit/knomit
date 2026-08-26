// Package synthesize — mid-session refresh of already-queued work items.
//
// A session's work items are MATERIALISED when the session is planned: each
// item's payload is a snapshot of the facts it was about, marshalled into
// facts_json and handed to the prompt verbatim at render time. That is fine
// while the corpus holds still, and a review session is precisely the thing
// that stops it holding still — a merge applied to one item retires facts that
// later items are still carrying.
//
// Corpus-level retirement itself WORKS: the merged fact is written and every
// source is deleted, so a session that starts afterwards never sees the
// originals. The gap is only intra-session: items materialised before the merge
// are never re-checked against it, so the same session goes on to re-offer a
// fact that no longer exists — spending a judge slot, or one of the eight
// backfill slots, on a corpus state that is already gone.
//
// This file closes that gap. It takes the set of paths an apply RETIRED and
// brings the still-queued items back into agreement with the corpus.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

// minJudgeableMembers is the smallest payload a consolidation judge can act on.
//
// A STRUCTURAL floor, not a corpus property (MN13): a prune item asks "should
// these facts be merged", and that question is unanswerable about one fact.
// The same floor already governs planning — Plan filters to clusters with more
// than one member, and enqueueRestatementItems drops a pair it can only half
// ship.
const minJudgeableMembers = 2

// minBridgeMembers is the same kind of structural floor for a bridge: a
// discover item asks what two or more independent facts jointly entail, and
// enumerateBridgeCandidates will not emit a set below it.
const minBridgeMembers = 2

// refreshInFlightItems reconciles this session's UNANSWERED work items with a
// set of paths that have just been retired from the corpus.
//
// Composability note: this takes the retired set as a whole, AFTER the apply
// that produced it has finished, rather than hooking each individual delete.
// That is deliberate — it works unchanged whether the merge writes and deletes
// sequentially or lands both halves in one atomic batch commit.
//
// Per-item failures warn and skip that item (conventions/synthesize/write-paths/
// failure-isolation): a payload that cannot be decoded is one stale item, and
// losing the whole refresh over it would strand every other item too.
func refreshInFlightItems(ctx context.Context, d Deps, sess *store.PipelineSession, branch string, retired []string) error {
	if len(retired) == 0 || sess == nil {
		return nil
	}
	gone := make(map[string]struct{}, len(retired))
	for _, p := range retired {
		gone[p] = struct{}{}
	}

	items, err := d.Pipeline.PendingPipelineWorkItems(ctx, sess.ID)
	if err != nil {
		return wrapf(reviewTool, err, "in-flight refresh: read pending items")
	}

	var updated, dropped int
	for _, item := range items {
		action, payload, aerr := refreshedPayload(ctx, d, branch, item, gone)
		if aerr != nil {
			log.Warn().Err(aerr).Int64("item", item.ID).Str("step", item.StepType).
				Msg("review: in-flight refresh could not read an item's payload; leaving it as planned")
			continue
		}
		switch action {
		case refreshUnchanged:
			continue
		case refreshRewrite:
			ok, uerr := d.Pipeline.UpdatePipelineWorkItemFacts(ctx, item.ID, payload)
			if uerr != nil {
				log.Warn().Err(uerr).Int64("item", item.ID).Msg("review: in-flight refresh: rewrite failed")
				continue
			}
			if ok {
				updated++
			}
		case refreshDelete:
			ok, derr := d.Pipeline.DeletePipelineWorkItem(ctx, item.ID)
			if derr != nil {
				log.Warn().Err(derr).Int64("item", item.ID).Msg("review: in-flight refresh: delete failed")
				continue
			}
			if ok {
				dropped++
			}
		}
	}

	if updated > 0 || dropped > 0 {
		log.Info().Str("session", sess.ID).Int("retired", len(retired)).
			Int("items_rewritten", updated).Int("items_dropped", dropped).
			Msg("review: in-flight work items refreshed after a mid-session retirement")
		// Reported, not merely logged: an item that silently vanishes from the
		// queue is indistinguishable from one that was never planned.
		d.OnProgress(ProgressEvent{Phase: "detail-refresh", Message: fmt.Sprintf(
			"in-flight refresh: %d fact(s) retired mid-session — %d queued item(s) rewritten, %d dropped",
			len(retired), updated, dropped)})
	}
	return nil
}

// refreshAction is what a refresh decided about one item.
type refreshAction int

const (
	refreshUnchanged refreshAction = iota
	refreshRewrite
	refreshDelete
)

// refreshedPayload decides what to do with one queued item, and returns its
// rewritten payload when the answer is "rewrite".
//
// Step types with no fact payload — reflect, and the two motif VOCABULARY
// passes, which name motifs rather than facts — fall through unchanged. That
// is not an oversight to be tidied later: a pass whose payload cannot name a
// retired fact has nothing to reconcile.
func refreshedPayload(ctx context.Context, d Deps, branch string, item store.PipelineWorkItem, gone map[string]struct{}) (refreshAction, string, error) {
	switch item.StepType {
	case "prune":
		// Cluster prunes and shortlist pairs share this shape, and share the
		// floor: both ask whether these facts consolidate.
		return refreshFactList(item.FactsJSON, gone, minJudgeableMembers)

	case "distill":
		// Distill synthesizes UPWARD from what it is shown rather than
		// reconciling members against each other, so one surviving fact is
		// still a question worth asking. Only an empty payload is vacuous.
		return refreshFactList(item.FactsJSON, gone, 1)

	case "discover":
		var payload DiscoverWorkPayload
		if err := json.Unmarshal([]byte(item.FactsJSON), &payload); err != nil {
			return refreshUnchanged, "", fmt.Errorf("decode discover payload: %w", err)
		}
		kept, removed := withoutRetired(payload.Bridge.Members, gone)
		if removed == 0 {
			return refreshUnchanged, "", nil
		}
		if len(kept) < minBridgeMembers {
			return refreshDelete, "", nil
		}
		payload.Bridge.Members = kept
		blob, err := json.Marshal(payload)
		if err != nil {
			return refreshUnchanged, "", fmt.Errorf("re-marshal discover payload: %w", err)
		}
		return refreshRewrite, string(blob), nil

	case motifBackfillStepType:
		// Backfill is RE-MATERIALISED rather than filtered. Its payload is a
		// budget — the oldest maxBackfillFacts facts still lacking a motif —
		// so merely dropping a retired entry would leave the session offering
		// seven where it had eight. Re-deriving hands the freed slot to a fact
		// that still exists, which is the whole point of the sequence: a
		// confirmed duplicate must not cost backfill budget.
		if !backfillPayloadNames(item.FactsJSON, gone) {
			return refreshUnchanged, "", nil
		}
		payload, err := backfillPayloadFor(ctx, d, branch)
		if err != nil {
			return refreshUnchanged, "", err
		}
		if len(payload.Facts) == 0 {
			return refreshDelete, "", nil
		}
		blob, err := json.Marshal(payload)
		if err != nil {
			return refreshUnchanged, "", fmt.Errorf("re-marshal backfill payload: %w", err)
		}
		return refreshRewrite, string(blob), nil
	}
	return refreshUnchanged, "", nil
}

// refreshFactList reconciles a plain []factForLLM payload against the retired
// set, deleting the item when too little is left to judge.
func refreshFactList(factsJSON string, gone map[string]struct{}, floor int) (refreshAction, string, error) {
	var facts []factForLLM
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return refreshUnchanged, "", fmt.Errorf("decode fact list payload: %w", err)
	}
	kept, removed := withoutRetired(facts, gone)
	if removed == 0 {
		return refreshUnchanged, "", nil
	}
	if len(kept) < floor {
		return refreshDelete, "", nil
	}
	blob, err := json.Marshal(kept)
	if err != nil {
		return refreshUnchanged, "", fmt.Errorf("re-marshal fact list payload: %w", err)
	}
	return refreshRewrite, string(blob), nil
}

// withoutRetired returns the members that survive, and how many were removed.
func withoutRetired(facts []factForLLM, gone map[string]struct{}) ([]factForLLM, int) {
	kept := make([]factForLLM, 0, len(facts))
	for _, f := range facts {
		if _, dead := gone[f.File]; dead {
			continue
		}
		kept = append(kept, f)
	}
	return kept, len(facts) - len(kept)
}

// backfillPayloadNames reports whether a backfill payload offers any of the
// retired paths. Checked before re-deriving, so an untouched backfill item is
// left exactly as planned rather than silently re-rolled on every merge.
func backfillPayloadNames(factsJSON string, gone map[string]struct{}) bool {
	var payload backfillPayload
	if err := json.Unmarshal([]byte(factsJSON), &payload); err != nil {
		return false
	}
	for _, f := range payload.Facts {
		if _, dead := gone[f.Path]; dead {
			return true
		}
	}
	return false
}
