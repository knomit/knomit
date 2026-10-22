// Package synthesize — the discover step, shared by the review (forward) and
// hypothesize (backward) strategies.
//
// It lives beside the engine rather than in strategy.go because it
// orchestrates: it writes facts, counts them into session stats and refreshes
// the session's queued items. Strategy.go holds only the vocabulary the
// engine and its strategies speak.
package synthesize

import (
	"context"
	"encoding/json"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/model"

	"github.com/rs/zerolog/log"
)

// ── shared "discover" step ────────────────────────────────────────────────
//
// The discover step is the one step type both the review (forward) and
// hypothesize (backward) strategies queue, and their handling of it is
// identical: the direction travels in the payload, and the verification gates
// come from the already-shared DiscoveryGatesFor. Keeping one implementation
// here rather than one per strategy is what stops the two directions from
// drifting apart in their gate handling — historically the failure mode that
// let a discovered fact seed its own discovery.

// discoverDecision is the discover step's decoded form.
//
// parsed is false when the response could not be parsed. That is deliberately
// NOT an error: discovery is non-fatal enrichment, and aborting would kill an
// in-progress session and lose its already-queued grounded work. The item is
// still claimed and applied as a no-op — a response that failed to parse will
// not parse on a retry either, so leaving it unanswered would wedge the
// session on an unanswerable item.
type discoverDecision struct {
	payload        DiscoverWorkPayload
	parsed         bool
	proposals      []DiscoveredFact
	reinforcements []FactReinforcement
}

// decodeDiscoverStep is the shared Decode half of the discover step. tool
// prefixes errors and logs so a mixed-tool log stays readable.
//
// A malformed *payload* IS fatal, unlike a malformed response: the payload is
// server-written, so failing to read it back means the queue is corrupt, not
// that the agent answered badly.
func decodeDiscoverStep(tool string, item *model.PipelineWorkItem, response string) (*discoverDecision, error) {
	var payload DiscoverWorkPayload
	if err := json.Unmarshal([]byte(item.FactsJSON), &payload); err != nil {
		return nil, wrapf(tool, err, "unmarshal discover payload")
	}
	d := &discoverDecision{payload: payload}
	parsed, perr := parseDiscoverResponse(response)
	if perr != nil {
		log.Warn().Err(perr).Str("tool", tool).Str("session", item.SessionID).
			Msg("discover response parse failed; treating as no-op")
		return d, nil
	}
	d.parsed = true
	d.proposals = parsed.Proposals
	d.reinforcements = parsed.Reinforcements
	return d, nil
}

// applyDiscoverStep is the shared Apply half of the discover step. Every
// failure below is logged and swallowed: a failure deep in the gate chain must
// not abort an in-progress session and lose its queued grounded work. Bad
// proposals are dropped, good ones land.
//
// The branch comes from sess.Branch, never from d.RI.AgentBranch().
func applyDiscoverStep(ctx context.Context, tool string, d Deps, sess *model.PipelineSession, dec *discoverDecision) error {
	if !dec.parsed {
		return nil
	}
	gates := DiscoveryGatesFor(d.RI, dec.payload.Direction)
	written, err := applyDiscoveredProposals(ctx, d.Facts, d.Files, d.Search, d.RI.Embedder(),
		dec.payload, dec.proposals, gates, sess.Branch, fact.ID12(d.RI.ID()), d.RI.OntologyRoot(), d.OnProgress)
	if err != nil {
		log.Warn().Err(err).Str("tool", tool).Str("session", sess.ID).
			Msg("apply discover failed; continuing")
	}
	// Count what landed, even on the error path — a partial apply still
	// changed the corpus. Without this the discover path wrote facts that no
	// counter saw, and a session that discovered but neither pruned nor
	// distilled reported all zeros: a summary that reads as "nothing to do"
	// over work that was actually done. Forward discovery writes synthesis
	// facts, so Synthesized is the honest bucket; backward (hypothesize)
	// writes hypotheses, which the same field counts as facts this session
	// created rather than leaving invisible.
	if len(written) > 0 {
		recordStats(ctx, tool, d, sess, &ReviewStats{Synthesized: len(written)})
	}

	// REINFORCE runs after the proposals, on the same decision. An agent that
	// found the corpus already holds its keystone returns no proposal and one
	// reinforcement, so this is usually the only half that does anything.
	// No error return: every rejection inside is per-reinforcement, warned and
	// skipped, exactly like the proposal loop above. It returned one until the
	// Phase-3 review (L7) pointed out the branch handling it was unreachable —
	// a handled failure mode that did not exist reads as a risk that was
	// considered, which is worse than no branch at all.
	reinforced := applyReinforcements(ctx, d.Facts, d.Files, d.Search, dec.payload,
		dec.reinforcements, sess.Branch, fact.ID12(d.RI.ID()), d.OnProgress)
	if len(reinforced) > 0 {
		// Reinforcement REWRITES a live fact — it bumps sources and adds a ref
		// — so every item still queued in this session is carrying a stale
		// snapshot of it. Discover has no retire path of its own (it writes and
		// reinforces; nothing here deletes), so this is the whole of discover's
		// contribution to in-session staleness, and it is the mutated-but-live
		// half rather than the retired half.
		if rerr := refreshInFlightItems(ctx, d, sess, sess.Branch,
			&ReviewStats{Rewritten: reinforced}); rerr != nil {
			log.Warn().Err(rerr).Str("tool", tool).Str("session", sess.ID).
				Msg("in-flight work items could not be refreshed after reinforcement")
		}
		// Deliberately NOT counted into ReviewStats. Reinforcement creates no
		// fact, so counting it as Synthesized would overstate what the session
		// added; and model.PipelineSessionStats has no column of its own for
		// it, so a new ReviewStats field would be dropped silently by
		// recordStats — recorded-looking and gone. It surfaces as a
		// detail-discover progress event per fact, and as this line.
		log.Info().Str("tool", tool).Str("session", sess.ID).
			Strs("facts", reinforced).Msg("discover: reinforced existing facts")
	}
	return nil
}

// enqueueDiscover writes one discover work item. One definition, so the
// entity/domain path and both motif lanes cannot drift apart in how an item is
// shaped, keyed, or prioritised.
func enqueueDiscover(ctx context.Context, d Deps, sess *model.PipelineSession, payload DiscoverWorkPayload, clusterKey string, priority float64) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return wrapf(reviewTool, err, "marshal discover payload %s", clusterKey)
	}
	if err := d.Pipeline.InsertPipelineWorkItem(ctx, model.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "discover",
		ClusterKey: clusterKey,
		FactsJSON:  string(payloadJSON),
		Priority:   priority,
	}); err != nil {
		return wrapf(reviewTool, err, "insert discover item %s", clusterKey)
	}
	return nil
}
