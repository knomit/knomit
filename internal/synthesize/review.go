// Package synthesize — Reviewer: the review tool's facade over the shared
// synthesis engine.
//
// Reviewer used to BE the review orchestrator; it is now a thin adapter that
// constructs a Pipeline over reviewStrategy and converts the engine's
// tool-neutral PipelineResult into the review wire types (ReviewResult,
// ReviewItem, ReviewProgress, ReviewStats). The engine lives in pipeline.go
// and the review-specific behaviour in review_strategy.go.
//
// The facade is not vestigial: it is the review tool's stable public surface.
// internal/mcp/review.go and internal/web/handlers_jobs.go both construct a
// Reviewer, and the wire types are what MCP clients see.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog/log"

	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Reviewer orchestrates multi-turn review sessions.
//
// Like the Pipeline it wraps, Reviewer is a per-call object: the MCP handler
// constructs a fresh one per call, so any per-session field on this struct
// would silently lose state between calls. Session-scoped state — including
// whether the reflect step has been considered for a given session — lives on
// the pipeline_sessions row (invariants/synthesize/
// per-call-objects-no-session-state).
type Reviewer struct {
	// ri is retained alongside the engine's own copy so callers that hold a
	// Reviewer can build another object against the same repo. It is repo
	// identity, not session state, so it is safe to keep here.
	ri *repos.RepoInstance
	p  *Pipeline
}

// NewReviewer creates a new review orchestrator at the default effort
// (normal — zero discovery spend). Use NewReviewerWithEffort to opt into the
// medium/high discovery dial.
//
// Effort is a resource dial, not a functionality freeze: normal is the
// cheapest rung, and its only contract is that the optional discovery
// machinery stays switched off (see invariants/synthesize/
// effort-normal-byte-identical). Changes that apply uniformly at every effort
// level are not constrained by it.
//
// ScopedCluster clusters the review subgraph in-process via
// SearchQuery.SubgraphEdges on the per-repo Service; no cluster cache is
// threaded through the synthesize layer.
func NewReviewer(ri *repos.RepoInstance, onProgress func(ProgressEvent)) *Reviewer {
	return NewReviewerWithEffort(ri, onProgress, DefaultEffort)
}

// NewReviewerWithEffort is the explicit-effort form. Empty effort defaults to
// DefaultEffort. Validation is the MCP layer's job; this constructor accepts
// whatever value it gets (review starts up; bad efforts surface as no-op
// discovery, not panics).
//
// Use NewReviewerWithOptions to also pass a ScopeFilter.
func NewReviewerWithEffort(ri *repos.RepoInstance, onProgress func(ProgressEvent), effort Effort) *Reviewer {
	return NewReviewerWithOptions(ri, onProgress, effort, ScopeFilter{})
}

// NewReviewerWithOptions is the full form: effort + optional scope filter.
func NewReviewerWithOptions(ri *repos.RepoInstance, onProgress func(ProgressEvent), effort Effort, scope ScopeFilter) *Reviewer {
	return &Reviewer{ri: ri, p: NewPipeline(ri, onProgress, effort, scope, reviewStrategy{})}
}

// Effort returns the discovery dial this Reviewer was constructed with.
// Exposed so the MCP layer can log/expose the resolved effort back to clients.
func (r *Reviewer) Effort() Effort { return r.p.Effort() }

// StartSession creates a new review session, identifies dirty facts, clusters
// them, stores work items, and returns the first item to review.
//
// This is the boundary at which the agent branch is bound to the session: the
// value of ri.AgentBranch() at this moment becomes sess.Branch and travels
// with the session for the rest of its lifetime. Nothing downstream reads
// ri.AgentBranch() again (invariants/synthesize/session-branch-binding).
func (r *Reviewer) StartSession(ctx context.Context) (*ReviewResult, error) {
	return reviewResult(r.p.StartSession(ctx))
}

// ContinueSession processes the model's response for the current work item
// and returns the next item, or done if the session is complete.
//
// Equivalent to ContinueSessionForItem with itemID 0 (no item assertion).
// Kept as the plain form because most callers — RunAll, the tests, and any
// client that predates the item_id wire field — have no id to assert.
func (r *Reviewer) ContinueSession(ctx context.Context, sessionID, response string) (*ReviewResult, error) {
	return reviewResult(r.p.ContinueSession(ctx, sessionID, response))
}

// ContinueSessionForItem is ContinueSession with an optional assertion that
// the response belongs to work item itemID. Pass 0 to skip the assertion.
// See Pipeline.ContinueSessionForItem for the peek → decode → claim → apply
// ordering and why it is ordered that way.
func (r *Reviewer) ContinueSessionForItem(ctx context.Context, sessionID, response string, itemID int64) (*ReviewResult, error) {
	return reviewResult(r.p.ContinueSessionForItem(ctx, sessionID, response, itemID))
}

// ContinueSessionForItemPaged is ContinueSessionForItem carrying the
// completion token read off a multi-page item's final page. Single-page items
// ignore it, so passing "" is correct for every caller that never paged.
func (r *Reviewer) ContinueSessionForItemPaged(ctx context.Context, sessionID, response string, itemID int64, completionToken string) (*ReviewResult, error) {
	return reviewResult(r.p.ContinueSessionForItemPaged(ctx, sessionID, response, itemID, completionToken))
}

// PageItem serves one page of the item currently outstanding on a session.
//
// A pure read: it neither answers the item nor advances the session, so an
// agent may fetch pages in any order, re-fetch them, or abandon the item
// entirely without consequence. That is what keeps
// invariants/synthesize/work-item-claim-protocol intact — the claim CAS still
// fires exactly once, on the response, never on a page fetch.
//
// itemID is asserted when non-zero, for the same reason ContinueSessionForItem
// asserts it: paging a different item than the agent believes it is reading
// would assemble one synthesis out of two items' facts.
func (r *Reviewer) PageItem(ctx context.Context, sessionID string, itemID int64, page int) (*ReviewResult, error) {
	res, err := r.p.CurrentItem(ctx, sessionID, itemID, page)
	if err != nil {
		return nil, err
	}
	return reviewResultPage(res, page)
}

// RunAll drives the review session to completion using an LLM adapter.
func (r *Reviewer) RunAll(ctx context.Context, adapter llm.LLMAdapter) error {
	return r.p.RunAll(ctx, adapter)
}

// reviewResult converts the engine's tool-neutral turn result into the review
// wire shape. Written to take the (result, error) pair so every delegating
// method above stays a one-liner.
//
// The engine's PipelineItem carries two payload fields and they are not
// interchangeable. FactsJSON is the raw STORED row, dropped here — hypothesize
// echoes that one back verbatim, review does not. Facts is what the strategy
// chose to RENDER beside the prompt, and it becomes ReviewItem.Facts.
func reviewResult(res *PipelineResult, err error) (*ReviewResult, error) {
	if err != nil {
		return nil, err
	}
	return reviewResultPage(res, 1)
}

// reviewResultPage is reviewResult for a chosen page.
//
// Paging lives HERE, at the transport boundary, and not in the strategy's
// Render: the engine and its in-process consumers (RunAll, the web synthesis
// job) want the whole item, and only the MCP wire has a per-result size limit.
// Slicing in Render would have handed RunAll one page and silently dropped the
// rest — the model would receive instructions about facts it was never shown.
func reviewResultPage(res *PipelineResult, page int) (*ReviewResult, error) {
	if res == nil {
		return nil, nil
	}
	out := &ReviewResult{
		SessionID: res.SessionID,
		Done:      res.Done,
		Summary:   res.Summary,
		Progress:  res.Progress,
		Health:    res.Health,
	}
	if res.Item == nil {
		return out, nil
	}

	out.Item = &ReviewItem{
		ID:   res.Item.ID,
		Type: res.Item.Type,
	}
	if res.Item.Facts == "" {
		// Nothing shipped beside the prompt: a single-page step type.
		out.Item.Prompt = res.Item.Prompt
		out.Item.ResponseSchema = res.Item.ResponseSchema
		out.Item.Page, out.Item.Pages = 1, 1
		return out, nil
	}

	pages, err := factPages(res.Item.Facts)
	if err != nil {
		return nil, err
	}
	if page < 1 {
		page = 1
	}
	if page > len(pages) {
		return nil, fmt.Errorf("work item %d has %d page(s); page %d does not exist",
			res.Item.ID, len(pages), page)
	}

	out.Item.Facts = pages[page-1]
	out.Item.Page = page
	out.Item.Pages = len(pages)
	out.Item.MoreAvailable = page < len(pages)

	// Instructions and schema ride page 1 only — by the time page 2 arrives
	// they are already in the agent's context, and repeating them per page
	// would spend the very budget paging exists to protect.
	if page == 1 {
		out.Item.Prompt = res.Item.Prompt
		out.Item.ResponseSchema = res.Item.ResponseSchema
	}

	switch {
	case len(pages) == 1:
		// Single-page item: no token, nothing to accumulate, no protocol to
		// explain. Keeping this case silent is what stops paging leaking into
		// the overwhelming majority of items that never needed it.
	case out.Item.MoreAvailable:
		out.Item.Next = fmt.Sprintf(
			"Page %d of %d. Do NOT answer yet — call knomit_review again with session_id, item_id=%d and page=%d to continue reading this item.",
			page, len(pages), res.Item.ID, page+1)
	default:
		out.Item.CompletionToken = completionTokenFor(res.Item.ID, res.Item.Facts)
		out.Item.Next = fmt.Sprintf(
			"Final page (%d of %d). You have now seen every fact in this item. Submit your response with item_id=%d and completion_token=%q.",
			page, len(pages), res.Item.ID, out.Item.CompletionToken)
	}

	// Last line of defence, and deliberately a measurement of the finished
	// artifact rather than another prediction of it. maxPageFactBytes bounds the
	// facts, but two things on a page are not the pager's to bound: the prompt
	// (distill's methodology section grows with the retrieved titles) and a
	// single fact too large to split. Either can push a page over the cap, and
	// the failure mode is the one this whole area keeps hitting — the client
	// spills the result to disk and the agent sees nothing, with no error
	// anywhere. Say so in the log instead.
	if delivered, err := json.MarshalIndent(out, "", "  "); err == nil && len(delivered) > maxDeliveredItemBytes {
		log.Warn().Int64("item", res.Item.ID).Str("type", res.Item.Type).
			Int("page", page).Int("pages", len(pages)).
			Int("bytes", len(delivered)).Int("limit", maxDeliveredItemBytes).
			Msg("review: delivered page exceeds the tool-result budget; the client may reject it — check for an oversized single fact or an overlong prompt")
	}
	return out, nil
}

// ── internals retained for the review test suite ──────────────────────────
//
// The methods below delegate to the engine or to review_strategy.go. They
// exist because the review tests drive these seams directly — the seed scan,
// the phase dispatcher, and the two methodology loaders each have dedicated
// regression coverage that predates the engine extraction and asserts on
// review-shaped values. Keeping the adapters here lets those tests keep
// testing behaviour rather than being rewritten around the new seam.

// storeIndices returns the store indices under the repo read lock.
func (r *Reviewer) storeIndices() (store.FactIndex, SearchQuery, store.PipelineIndex, store.BranchIndex) {
	gs, idx, pipelineIdx, branches, _, _ := r.p.storeIndices()
	return gs, idx, pipelineIdx, branches
}

// dirtyFacts returns the review seed facts (changed since watermark, or the
// whole epistemic corpus on a full scan), projected onto the prompt-facing
// shape the review path works in.
func (r *Reviewer) dirtyFacts(ctx context.Context, branch string, gs store.FactIndex, idx SearchQuery, pipelineIdx store.PipelineIndex) ([]factForLLM, error) {
	seeds, err := r.p.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
	if err != nil {
		return nil, err
	}
	if seeds == nil {
		return nil, nil
	}
	return factsForLLM(seeds), nil
}

// nextItem dispatches on the session's persistent phase and returns the
// review-shaped result.
func (r *Reviewer) nextItem(ctx context.Context, sess *store.PipelineSession) (*ReviewResult, error) {
	return reviewResult(r.p.nextItem(ctx, sess))
}

// completeSession marks the session done and (unless the session row says it
// was scoped) advances the review watermark.
func (r *Reviewer) completeSession(ctx context.Context, sess *store.PipelineSession) (*ReviewResult, error) {
	return reviewResult(r.p.completeSession(ctx, sess))
}

// loadDistillMethodology retrieves methodology relevant to a distill item's
// input facts. branch is required (no implicit AgentBranch fallback).
func (r *Reviewer) loadDistillMethodology(ctx context.Context, branch string, facts []factForLLM) string {
	return distillMethodologySection(ctx, r.p.ri, branch, facts)
}

// loadReflectMethodology retrieves methodology relevant to a reflect item's
// recorded hypothesis transitions. branch is required.
func (r *Reviewer) loadReflectMethodology(ctx context.Context, branch string, transitionsJSON []byte) string {
	return reflectMethodologySection(ctx, r.p.ri, branch, transitionsJSON)
}

// compile-time assertion that the review strategy satisfies the engine seam.
var _ Strategy = reviewStrategy{}
