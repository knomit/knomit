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
// This file closes that gap. It takes the paths an apply MUTATED — retired, or
// changed in place — and brings the still-queued items back into agreement with
// the corpus.
//
// It runs over EVERY unanswered item in the session, not just items of the
// vehicle that did the mutating. Staleness is cross-vehicle by nature: a prune
// merge retires facts that a distill item queued in the same session is still
// carrying, and measured live, that happens as often as the within-vehicle
// case.
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
func refreshInFlightItems(ctx context.Context, d Deps, sess *store.PipelineSession, branch string, stats *ReviewStats) error {
	if sess == nil || stats == nil {
		return nil
	}
	if len(stats.Retired) == 0 && len(stats.Rewritten) == 0 {
		return nil
	}
	m := mutation{
		gone:    make(map[string]struct{}, len(stats.Retired)),
		changed: make(map[string]struct{}, len(stats.Rewritten)),
	}
	for _, p := range stats.Retired {
		m.gone[p] = struct{}{}
	}
	for _, p := range stats.Rewritten {
		m.changed[p] = struct{}{}
	}

	items, err := d.Pipeline.PendingPipelineWorkItems(ctx, sess.ID)
	if err != nil {
		return wrapf(reviewTool, err, "in-flight refresh: read pending items")
	}

	var updated, dropped int
	for _, item := range items {
		action, payload, aerr := refreshedPayload(ctx, d, branch, item, m)
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
		log.Info().Str("session", sess.ID).
			Int("retired", len(stats.Retired)).Int("rewritten", len(stats.Rewritten)).
			Int("items_rewritten", updated).Int("items_dropped", dropped).
			Msg("review: in-flight work items refreshed after a mid-session retirement")
		// Reported, not merely logged: an item that silently vanishes from the
		// queue is indistinguishable from one that was never planned.
		d.OnProgress(ProgressEvent{Phase: "detail-refresh", Message: fmt.Sprintf(
			"in-flight refresh: %d fact(s) retired and %d changed mid-session — "+
				"%d queued item(s) rewritten, %d dropped",
			len(stats.Retired), len(stats.Rewritten), updated, dropped)})
	}
	return nil
}

// mutation is what one apply did to the corpus, in the two shapes a queued
// item cares about: members that must be DROPPED, and members that must be
// RE-READ because their stored fields moved underneath the snapshot.
type mutation struct {
	gone    map[string]struct{}
	changed map[string]struct{}
}

// touches reports whether a path was mutated at all.
func (m mutation) touches(path string) bool {
	if _, ok := m.gone[path]; ok {
		return true
	}
	_, ok := m.changed[path]
	return ok
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
func refreshedPayload(ctx context.Context, d Deps, branch string, item store.PipelineWorkItem, m mutation) (refreshAction, string, error) {
	switch item.StepType {
	case "prune":
		// Cluster prunes and shortlist pairs share this shape, and share the
		// floor: both ask whether these facts consolidate.
		return refreshFactList(ctx, d, branch, item.FactsJSON, m, minJudgeableMembers)

	case "distill":
		// Distill synthesizes UPWARD from what it is shown rather than
		// reconciling members against each other, so one surviving fact is
		// still a question worth asking. Only an empty payload is vacuous.
		return refreshFactList(ctx, d, branch, item.FactsJSON, m, 1)

	case "discover":
		var payload DiscoverWorkPayload
		if err := json.Unmarshal([]byte(item.FactsJSON), &payload); err != nil {
			return refreshUnchanged, "", fmt.Errorf("decode discover payload: %w", err)
		}
		kept, dropped, rewritten := reconcileMembers(ctx, d, branch, payload.Bridge.Members, m)
		if dropped == 0 && rewritten == 0 {
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
	}
	return refreshUnchanged, "", nil
}

// refreshFactList reconciles a plain []factForLLM payload against one apply's
// mutations, deleting the item when too little is left to judge.
func refreshFactList(ctx context.Context, d Deps, branch, factsJSON string, m mutation, floor int) (refreshAction, string, error) {
	var facts []factForLLM
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return refreshUnchanged, "", fmt.Errorf("decode fact list payload: %w", err)
	}
	kept, dropped, rewritten := reconcileMembers(ctx, d, branch, facts, m)
	if dropped == 0 && rewritten == 0 {
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

// reconcileMembers drops retired members and RE-READS changed ones, returning
// the survivors plus how many were dropped and how many were re-read.
//
// Re-read rather than patched: the queued member is a snapshot of the fields a
// prompt shows, and an apply that rewrote the fact may have moved more than the
// one field it was asked about. Reading the live record is the only version of
// this that cannot drift from what a freshly planned item would have carried.
//
// A changed member that can no longer be read is left exactly as it was. That
// is the conservative direction: showing a stale snapshot costs one imprecise
// judgement, while dropping a member the corpus may still hold silently
// shrinks the question.
func reconcileMembers(ctx context.Context, d Deps, branch string, facts []factForLLM, m mutation) (kept []factForLLM, dropped, rewritten int) {
	kept = make([]factForLLM, 0, len(facts))
	for _, f := range facts {
		if _, dead := m.gone[f.File]; dead {
			dropped++
			continue
		}
		if _, moved := m.changed[f.File]; moved {
			if fresh, ok := liveMember(ctx, d, branch, f); ok {
				kept = append(kept, fresh)
				rewritten++
				continue
			}
		}
		kept = append(kept, f)
	}
	return kept, dropped, rewritten
}

// liveMember re-reads one member from the index, in the same field mapping the
// planner uses (enqueueRestatementItems). Motifs are carried too: a member's
// motifs steer the distill enrichment line and the bridge specificity score.
func liveMember(ctx context.Context, d Deps, branch string, stale factForLLM) (factForLLM, bool) {
	rec, err := d.Search.GetByPath(ctx, branch, stale.File)
	if err != nil || rec == nil {
		return stale, false
	}
	return factForLLM{
		File:       rec.Path,
		Title:      rec.Title,
		Body:       rec.Body,
		Type:       rec.Type,
		Domain:     rec.Domain,
		Entities:   rec.Entities,
		Motifs:     rec.Motifs,
		Confidence: rec.Confidence,
		Sources:    rec.Sources,
		Origin:     rec.Origin,
	}, true
}
