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
// store.SearchIndex.SubgraphEdges on the per-repo Service; no cluster cache is
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

// RunAll drives the review session to completion using an LLM adapter.
func (r *Reviewer) RunAll(ctx context.Context, adapter llm.LLMAdapter) error {
	return r.p.RunAll(ctx, adapter)
}

// reviewResult converts the engine's tool-neutral turn result into the review
// wire shape. Written to take the (result, error) pair so every delegating
// method above stays a one-liner.
//
// The engine's PipelineItem carries a FactsJSON field that ReviewItem has no
// place for; dropping it here is deliberate — review renders its payload into
// the prompt rather than shipping it raw the way hypothesize does.
func reviewResult(res *PipelineResult, err error) (*ReviewResult, error) {
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	out := &ReviewResult{
		SessionID: res.SessionID,
		Done:      res.Done,
		Summary:   res.Summary,
		Progress:  res.Progress,
	}
	if res.Item != nil {
		out.Item = &ReviewItem{
			ID:             res.Item.ID,
			Type:           res.Item.Type,
			Prompt:         res.Item.Prompt,
			ResponseSchema: res.Item.ResponseSchema,
		}
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
func (r *Reviewer) storeIndices() (store.FactIndex, store.SearchIndex, store.PipelineIndex, store.BranchIndex) {
	return r.p.storeIndices()
}

// dirtyFacts returns the review seed facts (changed since watermark, or the
// whole epistemic corpus on a full scan), projected onto the prompt-facing
// shape the review path works in.
func (r *Reviewer) dirtyFacts(ctx context.Context, branch string, gs store.FactIndex, idx store.SearchIndex, pipelineIdx store.PipelineIndex) ([]factForLLM, error) {
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
