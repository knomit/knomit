// Package synthesize — Pipeline: the tool-neutral synthesis engine.
//
// Pipeline drives a multi-turn synthesis session: it creates the session,
// scans for seed facts, asks its Strategy to plan work, then serves work items
// one at a time and applies the agent's answers until the queue drains and the
// session completes. Everything here is true of every synthesis tool; the
// per-tool parts live behind Strategy (strategy.go).
//
// Two structural rules shape this file, both load-bearing:
//
//   - No session-scoped state on Pipeline. The MCP layer builds a fresh
//     Pipeline per call, so a field that tracked "where is this session up to"
//     would be silently reset between turns. Session state lives on the
//     pipeline_sessions row — phase, scoped flag, stats — and is re-read each
//     turn (invariants/synthesize/per-call-objects-no-session-state).
//
//   - The branch is read from ri exactly once, in StartSession, and stored on
//     the session row. Every method below takes its branch from sess.Branch,
//     so a session that outlives a change of agent branch still operates on
//     the branch it was created against
//     (invariants/synthesize/session-branch-binding).
package synthesize

import (
	"context"
	"fmt"
	"strings"
	"time"

	"knomit/internal/fact"
	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
)

// Pipeline is the shared synthesis engine. It is a per-call object: construct
// one, run one turn, discard it. See the package comment above for why no
// session state may be added to this struct.
type Pipeline struct {
	ri         *repos.RepoInstance
	onProgress func(ProgressEvent)
	// effort is in-memory only and deliberately NOT persisted on the session
	// row: it is a resource dial the caller re-supplies per call, not part of
	// the session's identity (invariants/synthesize/
	// effort-normal-byte-identical). Its whole contract is that at
	// EffortNormal the discovery machinery stays switched off, which is
	// enforced inside the bridge builders, not here.
	effort   Effort
	scope    ScopeFilter
	strategy Strategy
}

// NewPipeline builds an engine around a strategy. A nil onProgress is
// replaced with a no-op so neither the engine nor any strategy has to guard.
func NewPipeline(ri *repos.RepoInstance, onProgress func(ProgressEvent), effort Effort, scope ScopeFilter, strategy Strategy) *Pipeline {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	return &Pipeline{
		ri:         ri,
		onProgress: onProgress,
		effort:     NormalizeEffort(effort),
		scope:      scope,
		strategy:   strategy,
	}
}

// Effort returns the discovery dial this Pipeline was constructed with.
func (p *Pipeline) Effort() Effort { return p.effort }

// wrapf and errf give every engine error a consistent `<tool>: <what>` prefix.
// The tool name is a runtime value now that one engine serves several tools,
// so the prefix cannot be a string literal the way it was in review.go.
func wrapf(tool string, err error, format string, args ...any) error {
	return fmt.Errorf("%s: %s: %w", tool, fmt.Sprintf(format, args...), err)
}

func errf(tool string, format string, args ...any) error {
	return fmt.Errorf("%s: %s", tool, fmt.Sprintf(format, args...))
}

// storeIndices returns the store indices under the repo read lock.
func (p *Pipeline) storeIndices() (store.FactIndex, SearchQuery, store.PipelineIndex, store.BranchIndex, store.AbstractionIndex) {
	var gs store.FactIndex
	var idx SearchQuery
	var pipelineIdx store.PipelineIndex
	var branches store.BranchIndex
	var abstraction store.AbstractionIndex
	p.ri.WithRead(func(svc *store.Service) {
		gs = svc.Facts()
		idx = svc.Search()
		pipelineIdx = svc.Pipeline()
		branches = svc.Branches()
		abstraction = svc.Abstraction()
	})
	return gs, idx, pipelineIdx, branches, abstraction
}

// deps resolves the per-call dependency bundle handed to Strategy methods.
// Resolved once per engine entry point (and once per dispatch hop) rather than
// cached on the struct, so a store swap between turns is picked up.
func (p *Pipeline) deps() Deps {
	gs, idx, pipelineIdx, branches, abstraction := p.storeIndices()
	return Deps{
		RI:          p.ri,
		Facts:       gs,
		Search:      idx,
		Pipeline:    pipelineIdx,
		Branches:    branches,
		Abstraction: abstraction,
		Effort:      p.effort,
		Scope:       p.scope,
		OnProgress:  p.onProgress,
	}
}

// StartSession creates a session, scans for seed facts, asks the strategy to
// plan work over them, and returns the first item.
//
// This is the ONLY place the engine reads ri.AgentBranch(). The value becomes
// sess.Branch and travels with the session for the rest of its lifetime; every
// method below reads it back off the row
// (invariants/synthesize/session-branch-binding).
func (p *Pipeline) StartSession(ctx context.Context) (*PipelineResult, error) {
	tool := p.strategy.Tool()
	totalStart := time.Now()
	d := p.deps()
	branch := p.ri.AgentBranch()

	sess, err := d.Pipeline.CreatePipelineSession(ctx, tool, branch)
	if err != nil {
		return nil, wrapf(tool, err, "create session")
	}

	// Persist the scoped flag on the session row so completeSession can
	// suppress watermark advancement, even though the MCP handler reconstructs
	// a fresh engine (with empty scope) on every continue call. Relying on the
	// in-memory p.scope would let the completing continue call — which carries
	// no domain/entities args — advance the watermark to HEAD and permanently
	// hide out-of-scope facts from future unscoped sessions. Fatal on error:
	// silently leaving Scoped=false reintroduces exactly that poisoning.
	if !p.scope.IsEmpty() {
		if err := d.Pipeline.MarkPipelineSessionScoped(ctx, sess.ID); err != nil {
			return nil, wrapf(tool, err, "mark session scoped")
		}
		sess.Scoped = true
	}

	t := time.Now()
	seeds, err := p.dirtyFacts(ctx, branch, d.Facts, d.Search, d.Pipeline)
	if err != nil {
		return nil, wrapf(tool, err, "dirty facts")
	}
	log.Info().Str("tool", tool).Str("session", sess.ID).Int("seeds", len(seeds)).
		Dur("elapsed", time.Since(t)).Msg("pipeline: seed scan")

	// An empty seed pool completes the session immediately — which still runs
	// the watermark advance, so an unscoped no-op run records that it saw HEAD.
	if len(seeds) == 0 {
		return p.completeSession(ctx, sess)
	}

	if err := p.strategy.Plan(ctx, d, sess, seeds); err != nil {
		return nil, err
	}

	log.Info().Str("tool", tool).Str("session", sess.ID).Int("seeds", len(seeds)).
		Str("effort", string(p.effort)).Dur("total", time.Since(totalStart)).
		Msg("pipeline: session started")

	res, err := p.nextItem(ctx, sess)
	if err != nil {
		return nil, err
	}
	// Health descriptors recorded during Plan ride the FIRST result: the
	// session row is the only place they could survive planning (the engine is
	// per-call stateless), and the start turn is the one the agent reads before
	// deciding how much of this session to work through.
	if fresh, ferr := d.Pipeline.GetPipelineSession(ctx, sess.ID); ferr == nil {
		res.Health = sessionHealthLines(fresh)
	}
	return res, nil
}

// ContinueSession processes the model's response for the current work item and
// returns the next item, or done if the session is complete.
//
// Equivalent to ContinueSessionForItem with itemID 0 (no item assertion).
func (p *Pipeline) ContinueSession(ctx context.Context, sessionID, response string) (*PipelineResult, error) {
	return p.ContinueSessionForItem(ctx, sessionID, response, 0)
}

// ContinueSessionForItem is ContinueSession with an optional assertion that
// the response belongs to work item itemID. Pass 0 to skip the assertion.
//
// The assertion exists because the queue can change between rendering an item
// and receiving its answer: applying a distill item enqueues RAPTOR follow-up
// items, so the highest-priority unanswered item a continue call peeks is not
// necessarily the one the client was shown. Without the check, a client
// answering a stale item would have its decisions applied to a *different*
// item — validated against the wrong input paths. A mismatch is an error and
// touches nothing, so the correct item stays answerable.
//
// The call is ordered peek → decode+validate → CAS-claim → apply, and that
// ordering is the engine's to own rather than any strategy's:
//
//   - Decoding before claiming keeps the common failure class (malformed LLM
//     JSON, paths outside the item's inputs) fully retryable — the item is
//     left unanswered and the agent can try again. This is why Strategy.Decode
//     is required to be pure.
//   - Claiming before applying is what makes a retry idempotent: the claim is
//     a CAS on `response IS NULL`, so a resubmitted response loses and its
//     mutations are skipped entirely. Applying first (the pre-fix order) let a
//     duplicate submission mint a second copy of the same synthesized facts.
//
// The deliberate tradeoff: a hard failure *during* apply loses that one item's
// decisions, because the item is already consumed and is not un-claimed. That
// is accepted — the corpus is left un-maintained rather than corrupted,
// whereas duplicate synthesis facts are corruption.
func (p *Pipeline) ContinueSessionForItem(ctx context.Context, sessionID, response string, itemID int64) (*PipelineResult, error) {
	return p.ContinueSessionForItemPaged(ctx, sessionID, response, itemID, "")
}

// itemDelivery records HOW the caller answering a work item received it. It is
// the only thing that decides whether the accumulate-then-respond guard below
// applies, because the hazard the guard exists for is a property of the
// DELIVERY, not of the item.
//
// The MCP wire splits an oversized item into pages, so an answer arriving over
// it is only trustworthy with proof the agent read every one. RunAll's
// in-process consumer is handed the whole item in a single Go value: there are
// no pages for it to miss, nothing on that path ever issues a token, and
// demanding one would make every multi-page item permanently unanswerable —
// which is precisely what it did.
//
// Deliberately unexported, and deliberately not a parameter on any exported
// method: if the wire could select deliveredWhole, the guard would be advisory
// again, which is the failure the guard was written to end.
type itemDelivery int

const (
	// deliveredByPage: the caller may have been served a slice of the item.
	deliveredByPage itemDelivery = iota
	// deliveredWhole: the caller holds every fact of the item by construction.
	deliveredWhole
)

// ContinueSessionForItemPaged is ContinueSessionForItem plus the
// accumulate-then-respond guard for multi-page items. completionToken is the
// value the agent read off the item's final page; it is ignored for items that
// fit one page.
func (p *Pipeline) ContinueSessionForItemPaged(ctx context.Context, sessionID, response string, itemID int64, completionToken string) (*PipelineResult, error) {
	return p.continueSessionForItem(ctx, sessionID, response, itemID, completionToken, deliveredByPage)
}

func (p *Pipeline) continueSessionForItem(ctx context.Context, sessionID, response string, itemID int64, completionToken string, delivery itemDelivery) (*PipelineResult, error) {
	tool := p.strategy.Tool()
	d := p.deps()

	sess, err := d.Pipeline.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, wrapf(tool, err, "get session")
	}
	if sess == nil {
		return nil, errf(tool, "session %q not found", sessionID)
	}
	if sess.Status != "active" {
		return nil, errf(tool, "session %q is %s, not active", sessionID, sess.Status)
	}

	item, err := d.Pipeline.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, wrapf(tool, err, "next work item")
	}
	if item == nil {
		// No unanswered items — let the dispatcher handle phase advancement
		// (work→reflect→done as appropriate). Don't short-circuit to
		// completeSession: that would skip the reflect phase entirely on
		// sessions where the last work item was just answered out-of-band.
		return p.nextItem(ctx, sess)
	}

	// D2 staleness guard. Rejecting outright — rather than silently answering
	// whatever is current — is the point: the response was reasoned about
	// against a different item's facts, so applying it here would validate it
	// against the wrong input paths.
	if itemID != 0 && itemID != item.ID {
		return nil, errf(tool, "response targets work item %d but item %d is current; "+
			"re-read the current item and answer that one", itemID, item.ID)
	}

	// Accumulate-then-respond guard, ahead of Decode and therefore ahead of the
	// claim: an answer to a multi-page item is only meaningful if the agent
	// actually read every page, and a rejection here leaves the item fully
	// retryable. Enforced rather than instructed — an advisory "page until
	// exhausted" would let an agent answer on page 1 and be ACCEPTED, turning a
	// loud transport failure into a silent quality loss. That is the lesson of
	// invariants/synthesize/response-envelope, where a schema's `required` list
	// meant nothing because no code probed for it.
	//
	// Asked of the strategy, not assumed: only strategies that ship a payload
	// beside the prompt can page, and hypothesize does not. Asked only of a
	// paged DELIVERY: see itemDelivery for why a caller holding the whole item
	// has nothing to prove.
	if ps, ok := p.strategy.(pagedStrategy); ok && delivery == deliveredByPage {
		if err := ps.RequireCompletion(item, completionToken); err != nil {
			return nil, wrapf(tool, err, "answer item %d", item.ID)
		}
	}

	// Decode and validate first. Every error below this point and above the
	// claim leaves the item unanswered, so the agent can retry.
	dec, normalized, err := p.strategy.Decode(item, response)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		// The claim CAS keys on `response IS NULL`; an empty response would
		// still win it, but storing empty text muddies the "was this answered"
		// read for anything that inspects the row. Strategies that accept
		// contentless answers substitute a placeholder in Decode; this guard
		// catches one that forgot.
		return nil, errf(tool, "strategy produced an empty stored response for item %d", item.ID)
	}

	// Claim. Losing the CAS means this item was already answered — by a
	// concurrent caller, or by an earlier attempt of this very submission
	// whose response reached the DB. Its mutations are already applied, so
	// re-applying them here is exactly the duplication P0.4 exists to kill.
	claimed, err := d.Pipeline.AnswerPipelineWorkItem(ctx, item.ID, normalized)
	if err != nil {
		// UNCOVERED: no test exercises this branch, and there is currently no
		// seam to build one on. Faulting the answer-write needs a store that
		// returns an error on demand, but the engine reaches its indices through
		// ri.WithRead, which hands out a concrete *store.Service — not an
		// interface — and repos.TestInstanceConfig accepts nothing else, so a
		// fault-injecting PipelineIndex cannot be threaded in. The deleted
		// internal/mcp TestHypothesizeContinue_MarkAnsweredBeforeApply covered it
		// by mocking PipelineIndex back when the MCP layer owned the claim.
		// Restoring it means making the store injectable at the repos boundary.
		//
		// What must hold if this ever regresses: the error propagates and Apply
		// below is NOT reached, so a failed claim writes no facts.
		return nil, wrapf(tool, err, "answer work item")
	}
	if !claimed {
		log.Info().Str("tool", tool).Str("session", sessionID).Int64("item", item.ID).
			Msg("pipeline: work item already answered; skipping apply")
		return p.nextItem(ctx, sess)
	}

	if err := p.strategy.Apply(ctx, d, sess, item, dec); err != nil {
		return nil, err
	}

	return p.nextItem(ctx, sess)
}

// RunAll drives the session to completion using an LLM adapter: start, then
// loop render prompt → LLM call → apply response until the queue drains.
func (p *Pipeline) RunAll(ctx context.Context, adapter llm.LLMAdapter) error {
	result, err := p.StartSession(ctx)
	if err != nil {
		return fmt.Errorf("RunAll: start: %w", err)
	}
	if result.Done {
		return nil
	}

	sessionID := result.SessionID
	for result.Item != nil {
		p.onProgress(ProgressEvent{
			Phase:   "llm",
			Message: fmt.Sprintf("processing %s work item", result.Item.Type),
		})

		// A step type that ships its payload beside the prompt (rather than
		// interpolated into it) must have the two recombined here: an LLM
		// adapter takes one message, and there is no second channel to put
		// facts on. Omitting this would hand the model instructions about
		// facts it was never shown — a silent, total loss of the item's
		// content that no error surfaces.
		//
		// This is also what makes the answer below a deliveredWhole one: the
		// model is shown every fact of the item in one message, so there is no
		// page it could have missed.
		content := result.Item.Prompt
		if result.Item.Facts != "" {
			content += "\n\nFacts in scope:\n" + result.Item.Facts
		}

		opts := llm.CompletionOptions{ForceJSON: true}
		response, err := adapter.Complete(ctx, "", []llm.Message{
			{Role: "user", Content: content},
		}, opts, nil)
		if err != nil {
			return fmt.Errorf("RunAll: LLM %s: %w", result.Item.Type, err)
		}

		result, err = p.continueSessionForItem(ctx, sessionID, response, 0, "", deliveredWhole)
		if err != nil {
			return fmt.Errorf("RunAll: continue: %w", err)
		}
	}
	return nil
}

// ── seed scan ─────────────────────────────────────────────────────────────

// dirtyFacts returns the session's seed facts: everything changed since this
// tool's watermark, or the whole (strategy-filtered) corpus on a full scan.
//
// First run (no watermark): uses the search index to retrieve facts without
// reading every file from git.
// Incremental (has watermark): uses DiffFiles to read only changed paths.
//
// The canonical seed type is fact.Fact rather than any per-tool projection,
// so the two scan paths converge on one shape and Strategy.AcceptSeed has one
// definition to be written against. Origin is copied on BOTH paths and is
// load-bearing: discovery seeding excludes origin=discovered facts, and
// dropping Origin here previously let a discovered fact seed its own
// discovery.
func (p *Pipeline) dirtyFacts(ctx context.Context, branch string, gs store.FactIndex, idx SearchQuery, pipelineIdx store.PipelineIndex) ([]fact.Fact, error) {
	tool := p.strategy.Tool()

	watermark, err := pipelineIdx.GetPipelineWatermark(ctx, tool, branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	// Full-scan path, taken when EITHER:
	//   - no watermark → first run, all facts are dirty; or
	//   - a scope filter is active → a scoped run is an on-demand pass over a
	//     slice of the corpus, independent of incremental change-tracking.
	//     Scoped sessions deliberately do NOT advance the watermark (see
	//     completeSession), so they must not be BLOCKED by it either: gating a
	//     scoped run on the shared watermark means that once a prior unscoped
	//     run pushed it to HEAD, every scoped run would diff an empty
	//     changeset and find zero seeds. Read and write sides must agree —
	//     scoped is exempt from both
	//     (decisions/architecture/synthesize/scope-filter).
	if watermark == "" || !p.scope.IsEmpty() {
		// Scope is applied in Go via p.scope.Matches, NOT pushed into
		// SearchOptions: store.Search ANDs its domain+entity clauses
		// (intersection) and canonicalises domains, whereas the filter is
		// union with raw membership. Routing both first-run and incremental
		// seeding through Matches keeps one definition of scope membership, so
		// the same scope yields the same seed pool regardless of watermark.
		results, err := idx.Search(ctx, branch, p.strategy.SeedQuery())
		if err != nil {
			return nil, fmt.Errorf("search all: %w", err)
		}
		seeds := make([]fact.Fact, 0, len(results))
		for _, sr := range results {
			f := factFromSearchResult(sr)
			if !p.strategy.AcceptSeed(f) {
				continue
			}
			if !p.scope.Matches(f.Domain, f.Entities) {
				continue
			}
			seeds = append(seeds, f)
		}
		return seeds, nil
	}

	// Incremental: only changed facts since watermark.
	added, modified, _, err := gs.DiffFiles(ctx, branch, watermark)
	if err != nil {
		return nil, fmt.Errorf("diff files: %w", err)
	}

	var seeds []fact.Fact
	for _, path := range append(added, modified...) {
		if !strings.HasSuffix(path, ".md") {
			continue
		}
		result, err := gs.ReadFact(ctx, branch, path, nil)
		if err != nil {
			continue // deleted or unreadable
		}
		f, err := fact.ParseFact(path, result.Content)
		if err != nil {
			continue // not a valid fact
		}
		if !p.strategy.AcceptSeed(f) {
			continue
		}
		if !p.scope.Matches(f.Domain, f.Entities) {
			continue
		}
		seeds = append(seeds, f)
	}
	return seeds, nil
}

// factFromSearchResult projects a search hit into a fact.Fact so the full-scan
// seed path yields the same type as the incremental (fact.ParseFact) path.
//
// Kind is normalized the way fact.ParseFact normalizes it — an absent kind
// means the default, epistemic. Without that, a strategy whose AcceptSeed
// tests Kind would reject the entire full-scan pool while accepting the
// incremental one, making the seed pool depend on watermark state.
//
// Origin must be copied: see dirtyFacts.
func factFromSearchResult(sr store.SearchResult) fact.Fact {
	f := fact.NewFact(sr.Path)
	f.Title = sr.Title
	f.Body = sr.Body
	f.Kind = fact.Kind(sr.Kind)
	if f.Kind == "" {
		f.Kind = fact.DefaultKind
	}
	f.Type = fact.Type(sr.Type)
	f.Domain = sr.Domain
	f.Entities = sr.Entities
	f.Confidence = sr.Confidence
	f.Sources = sr.Sources
	f.Refs = sr.Refs
	f.Origin = fact.Origin(sr.Origin)
	return f
}

// ── phase machine ─────────────────────────────────────────────────────────

// CurrentItem re-renders the item currently outstanding on a session without
// answering it or advancing anything.
//
// This is the read half of paging. It deliberately does NOT fall through to
// nextItem when the queue is empty: nextItem advances the phase machine and can
// complete the session, which is exactly the side effect a page fetch must not
// have. An agent asking for a page of an item that is no longer current has
// lost its place, and being told so is more useful than being silently handed
// something else.
//
// itemID is asserted when non-zero — the same staleness guard the answer path
// applies, for the same reason: pages from two different items must never be
// accumulated into one synthesis.
//
// page is not used to slice anything here — that happens at the transport
// boundary — but it decides how MUCH of the item has to be built. Pages after
// the first discard the prompt and the schema, so rendering them is pure waste,
// and expensive waste: review's Render performs a methodology retrieval per
// fact in the item, so a P-page item cost P× that. Only page 1 pays it.
func (p *Pipeline) CurrentItem(ctx context.Context, sessionID string, itemID int64, page int) (*PipelineResult, error) {
	tool := p.strategy.Tool()
	d := p.deps()

	sess, err := d.Pipeline.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, wrapf(tool, err, "get session")
	}
	if sess == nil {
		return nil, errf(tool, "session %q not found", sessionID)
	}
	if sess.Status != "active" {
		return nil, errf(tool, "session %q is %s, not active", sessionID, sess.Status)
	}

	item, err := d.Pipeline.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, wrapf(tool, err, "current work item")
	}
	if item == nil {
		return nil, errf(tool, "session %q has no outstanding work item to page", sessionID)
	}
	if itemID != 0 && itemID != item.ID {
		return nil, errf(tool, "requested pages of work item %d but item %d is current; "+
			"re-read the current item from page 1", itemID, item.ID)
	}

	// Payload-only render for pages the prompt will be stripped from anyway.
	// A strategy that cannot produce its payload without a full render returns
	// "", and the fall-through below is then the only correct answer — a page
	// missing its facts is worse than a page that cost too much to build.
	if page > 1 {
		if ps, ok := p.strategy.(pagedStrategy); ok {
			facts, err := ps.RenderPayload(item)
			if err != nil {
				return nil, wrapf(tool, err, "render payload for page %d", page)
			}
			if facts != "" {
				return p.payloadResult(ctx, d, sess, item, facts)
			}
		}
	}
	return p.renderWorkItem(ctx, d, sess, item)
}

// payloadResult assembles what renderWorkItem would, minus the two fields a
// page after the first never carries: Prompt and ResponseSchema. Progress
// counts are still read — they are one cheap query and the agent tracks the
// session by them.
func (p *Pipeline) payloadResult(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem, facts string) (*PipelineResult, error) {
	tool := p.strategy.Tool()

	completed, remaining, err := d.Pipeline.PipelineWorkItemStats(ctx, sess.ID)
	if err != nil {
		return nil, wrapf(tool, err, "work item stats")
	}

	return &PipelineResult{
		SessionID: sess.ID,
		Item: &PipelineItem{
			ID:        item.ID,
			Type:      item.StepType,
			FactsJSON: item.FactsJSON,
			Facts:     facts,
		},
		Progress: &ReviewProgress{
			Completed: completed,
			Remaining: remaining,
		},
	}, nil
}

// nextItem dispatches on the session's persistent phase. It is intentionally
// short: all session-scoped state — including "have we considered enqueueing
// the reflect item for this session?" — lives on the pipeline_sessions row,
// not on this engine instance, because the MCP handler constructs a fresh one
// per call.
//
// sess.Branch is the branch this session was created against; it does not
// change as the user's live AgentBranch changes during the session.
func (p *Pipeline) nextItem(ctx context.Context, sess *store.PipelineSession) (*PipelineResult, error) {
	switch sess.Phase {
	case "work":
		return p.handlePhase(ctx, sess, "work", "reflect")
	case "reflect":
		return p.handlePhase(ctx, sess, "reflect", "done")
	case "done":
		return p.completeSession(ctx, sess)
	default:
		return nil, errf(p.strategy.Tool(), "unknown phase %q on session %s", sess.Phase, sess.ID)
	}
}

// handlePhase serves the next work item of the current phase if one remains;
// once the queue is empty it advances from→to and re-dispatches.
//
// The two non-terminal phases had separate handlers before the extraction and
// their bodies were identical apart from the phase names, so they are one
// function here. The advance is a CAS on the phase column, so concurrent
// continuations can't both fire the strategy hook: at most one caller's UPDATE
// matches the `from` guard. The other observes the row already advanced and
// falls through to the next phase's dispatch
// (architecture/store/pipeline-session-phase-cas — a losing CAS is a benign
// no-op, not an error).
func (p *Pipeline) handlePhase(ctx context.Context, sess *store.PipelineSession, from, to string) (*PipelineResult, error) {
	tool := p.strategy.Tool()
	d := p.deps()

	item, err := d.Pipeline.NextPipelineWorkItem(ctx, sess.ID)
	if err != nil {
		return nil, wrapf(tool, err, "next item")
	}
	if item != nil {
		return p.renderWorkItem(ctx, d, sess, item)
	}

	advanced, err := d.Pipeline.AdvancePipelineSessionPhase(ctx, sess.ID, from, to)
	if err != nil {
		return nil, wrapf(tool, err, "advance %s→%s", from, to)
	}
	if advanced {
		log.Info().Str("tool", tool).Str("session", sess.ID).Str("from", from).Str("to", to).
			Msg("pipeline: phase transition")
		// Only the CAS winner runs the hook, which is what makes an insert
		// made inside it at-most-once per session per transition.
		if err := p.strategy.OnPhaseAdvance(ctx, d, sess, from, to); err != nil {
			return nil, err
		}
	}
	return p.refetchAndDispatch(ctx, sess.ID)
}

// refetchAndDispatch reloads the session row and re-enters nextItem so the
// dispatcher sees the post-advance phase. Used after a phase transition or
// when the in-memory phase value may be stale.
//
// Re-reading rather than mutating the in-memory row is also what unifies the
// scoped-flag read across tools: completeSession always sees a freshly
// fetched row, so no caller needs a defensive re-read of its own.
func (p *Pipeline) refetchAndDispatch(ctx context.Context, sessionID string) (*PipelineResult, error) {
	tool := p.strategy.Tool()
	_, _, pipelineIdx, _, _ := p.storeIndices()
	fresh, err := pipelineIdx.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, wrapf(tool, err, "refetch session")
	}
	if fresh == nil {
		return nil, errf(tool, "session %q disappeared mid-dispatch", sessionID)
	}
	return p.nextItem(ctx, fresh)
}

// renderWorkItem turns a work item into a result: the strategy supplies the
// prompt and schema, the engine attaches the item id, payload, and progress
// counts. Attaching the id here rather than in each strategy is deliberate —
// the D2 staleness guard only works if every rendered item carries the id the
// client is expected to echo back.
func (p *Pipeline) renderWorkItem(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem) (*PipelineResult, error) {
	tool := p.strategy.Tool()

	view, err := p.strategy.Render(ctx, d, sess, item)
	if err != nil {
		return nil, err
	}

	completed, remaining, err := d.Pipeline.PipelineWorkItemStats(ctx, sess.ID)
	if err != nil {
		return nil, wrapf(tool, err, "work item stats")
	}

	return &PipelineResult{
		SessionID: sess.ID,
		Item: &PipelineItem{
			ID:             item.ID,
			Type:           view.Type,
			Prompt:         view.Prompt,
			ResponseSchema: view.ResponseSchema,
			FactsJSON:      item.FactsJSON,
			Facts:          view.Facts,
		},
		Progress: &ReviewProgress{
			Completed: completed,
			Remaining: remaining,
		},
	}, nil
}

// ── completion ────────────────────────────────────────────────────────────

// completeSession marks the session done and advances this tool's watermark.
// Branch comes from sess.Branch — the branch the session was created against —
// so the HEAD lookup and the watermark advance are consistent with the seed
// scan that opened the session.
func (p *Pipeline) completeSession(ctx context.Context, sess *store.PipelineSession) (*PipelineResult, error) {
	tool := p.strategy.Tool()
	d := p.deps()
	branch := sess.Branch

	if err := d.Pipeline.CompletePipelineSession(ctx, sess.ID); err != nil {
		return nil, wrapf(tool, err, "complete session")
	}

	// A scoped run only processed a subset of facts. Advancing the watermark
	// to HEAD would permanently hide facts outside the scope from future
	// unscoped sessions. Read the scoped flag off the session row (persisted
	// in StartSession) rather than p.scope: the MCP handler rebuilds the
	// engine with empty scope on the completing continue call, so p.scope is
	// unreliable here. This is the write half of the scoped exemption whose
	// read half is in dirtyFacts; the two must always agree.
	if !sess.Scoped {
		headHash, err := d.Branches.HeadCommit(ctx, branch)
		if err != nil {
			log.Warn().Err(err).Str("tool", tool).Msg("pipeline: could not get HEAD for watermark")
		} else if err := d.Pipeline.SetPipelineWatermark(ctx, tool, branch, headHash); err != nil {
			log.Warn().Err(err).Str("tool", tool).Msg("pipeline: could not advance watermark")
		}
	}

	completed, _, err := d.Pipeline.PipelineWorkItemStats(ctx, sess.ID)
	if err != nil {
		log.Warn().Err(err).Str("tool", tool).Msg("pipeline: could not get final stats")
	}

	// Re-read the row rather than trusting the `sess` we were handed: it was
	// fetched before this call's item was applied, so its counters are one item
	// stale. A failure here costs only the summary numbers, so it degrades to a
	// zero summary — the same thing clients got before stats existed.
	summary := &ReviewStats{}
	if fresh, err := d.Pipeline.GetPipelineSession(ctx, sess.ID); err != nil {
		log.Warn().Err(err).Str("tool", tool).Msg("pipeline: could not read session stats for summary")
	} else if fresh != nil {
		summary = &ReviewStats{
			Pruned:      fresh.Stats.Pruned,
			Merged:      fresh.Stats.Merged,
			Updated:     fresh.Stats.Updated,
			Synthesized: fresh.Stats.Synthesized,
		}
	}

	log.Info().Str("tool", tool).Str("session", sess.ID).Int("completed", completed).
		Msg("pipeline: session complete")
	p.onProgress(ProgressEvent{Phase: tool + "-done", Message: fmt.Sprintf("session %s complete", sess.ID)})

	return &PipelineResult{
		SessionID: sess.ID,
		Done:      true,
		Summary:   summary,
		Progress:  &ReviewProgress{Completed: completed, Remaining: 0},
	}, nil
}

// recordStats accumulates one applied item's counts onto the session row,
// which is where running totals have to live: the engine is rebuilt per call,
// so nothing kept in memory survives to completion.
//
// A failure is logged and swallowed, and stats accumulated for an item whose
// apply then fails are simply lost. Both are deliberate: the summary is
// informational, the mutations it describes are already committed, and the
// claim protocol already accepts losing a whole item's decisions to a mid-apply
// crash — so there is nothing here worth a transaction or a retry.
func recordStats(ctx context.Context, tool string, d Deps, sess *store.PipelineSession, stats *ReviewStats) {
	if stats == nil {
		return
	}
	if err := d.Pipeline.AddPipelineSessionStats(ctx, sess.ID, store.PipelineSessionStats{
		Pruned:      stats.Pruned,
		Merged:      stats.Merged,
		Updated:     stats.Updated,
		Synthesized: stats.Synthesized,
	}); err != nil {
		log.Warn().Err(err).Str("tool", tool).Str("session", sess.ID).
			Msg("pipeline: could not record session stats")
	}
}
