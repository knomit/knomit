// Package synthesize — the Strategy seam of the shared synthesis engine.
//
// Pipeline (pipeline.go) owns everything that is true of *any* multi-turn
// synthesis session: the session lifecycle, the work→reflect→done phase
// machine, the seed scan and its watermark gate, the claim protocol, and
// completion. Strategy owns the parts that differ per tool: which facts seed a
// run, what work items get enqueued from those seeds, how a response is
// decoded and applied, and how an item is rendered for the agent.
//
// The split is drawn so that a Strategy implementation can be a zero-size
// struct. Everything a strategy needs to touch the world arrives as an
// argument (Deps) or comes off the session row — never off the strategy's own
// fields. That is not a style preference: it is how
// invariants/synthesize/per-call-objects-no-session-state is enforced
// structurally rather than by comment. Both the engine and its strategy are
// rebuilt from scratch on every MCP call, so any field either one carried
// across calls would silently be lost.
package synthesize

import (
	"context"
	"encoding/json"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
)

// SearchQuery is the store query surface synthesize depends on: fact search
// (FactQuery), graph queries (GraphStore), and methodology matching
// (MethodologyMatcher). It deliberately excludes HistoryQuery — synthesize
// never reads commit-log/revision history — so a strategy cannot reach a
// history method through this handle. store.SearchIndex (the composite)
// satisfies it, as does the transitional *MockSearchIndex.
type SearchQuery interface {
	store.FactQuery
	store.GraphStore
	store.MethodologyMatcher
}

// Deps is the per-call bundle of everything a Strategy method may reach for:
// the resolved store indices, the repo instance (config + embedder), and the
// engine's own dials.
//
// It is passed by value into every Strategy method rather than being stored on
// the strategy, which is what lets strategies stay stateless. The store
// indices are resolved once per engine entry point under the repo read lock
// (see Pipeline.deps) so a single call never sees two different Services.
//
// Deps deliberately carries NO branch and NO session: a strategy reads the
// branch off the *store.PipelineSession it is handed, because that is the
// branch the session was bound to at creation. See
// invariants/synthesize/session-branch-binding — nothing below StartSession
// may consult RI.AgentBranch().
type Deps struct {
	RI         *repos.RepoInstance
	Facts      store.FactIndex
	Search     SearchQuery
	Pipeline   store.PipelineIndex
	Branches   store.BranchIndex
	Effort     Effort
	Scope      ScopeFilter
	OnProgress func(ProgressEvent)
}

// pagedStrategy is the optional half of Strategy: implemented only by
// strategies whose work items can be served across several tool results.
//
// Optional rather than part of Strategy because paging is not universal —
// hypothesize ships one fact per item and has nothing to page — and widening
// the required interface would force a meaningless implementation on every
// tool. The engine type-asserts; a strategy that does not implement this is
// simply never asked.
type pagedStrategy interface {
	// RequireCompletion rejects an answer to a multi-page item that does not
	// carry proof the agent read every page. Called before Decode and before
	// the claim, so returning an error leaves the item retryable.
	RequireCompletion(item *store.PipelineWorkItem, completionToken string) error
}

// WorkItemView is a strategy's rendering of one work item: what the agent is
// shown and what shape its answer must take. The engine wraps it with the
// item id and payload (see PipelineItem) — a strategy never has to remember to
// echo the id, which is what makes the D2 staleness guard un-forgettable.
type WorkItemView struct {
	// Type is the item kind reported to the client. It is normally
	// item.StepType, but stays a field so a strategy can present a step under
	// a different name than it is queued under.
	Type           string
	Prompt         string
	ResponseSchema string
	// Facts is the item's payload as a structured JSON array, carried beside
	// the prompt instead of interpolated into it. Empty for step types whose
	// template still inlines their payload.
	Facts string
}

// PipelineItem is the engine's tool-neutral work item. Each tool's MCP facade
// projects it onto its own wire type (ReviewItem, HypothesizeItem), which is
// why this carries the union of what those need rather than either one's
// exact shape.
type PipelineItem struct {
	ID             int64
	Type           string
	Prompt         string
	ResponseSchema string
	// FactsJSON is the item's raw stored payload. Review does not surface it;
	// hypothesize echoes it back to the agent as the `fact` field. Carrying it
	// here spares the facades a second read of the work item.
	FactsJSON string
	// Facts is the RENDERED payload the strategy chose to ship beside the
	// prompt — distinct from FactsJSON, which is what the row stores. They
	// coincide for distill today; keeping them separate is what lets a
	// strategy ship a projection (or, later, one page) of a larger stored
	// payload without the store shape leaking onto the wire.
	Facts string
}

// PipelineResult is the engine's tool-neutral turn result.
type PipelineResult struct {
	SessionID string
	Item      *PipelineItem
	Done      bool
	// Summary is populated only on the completing turn. It reuses ReviewStats
	// because that is the type already on the wire and in the session row;
	// the name is review-flavoured but the counters are not.
	Summary  *ReviewStats
	Progress *ReviewProgress
}

// Strategy is the per-tool half of a synthesis pipeline.
//
// The method set is ordered by when the engine calls it: Tool/SeedQuery/
// AcceptSeed during the seed scan, Plan once at start, then Decode/Apply per
// answered item, Render per served item, and OnPhaseAdvance whenever the
// engine wins a phase CAS.
type Strategy interface {
	// Tool is the pipeline_sessions.tool value and the watermark key. Sessions
	// and watermarks are namespaced by it, so two strategies with the same
	// Tool string would share (and corrupt) each other's incremental state.
	Tool() string

	// SeedQuery is the store query used by the full-scan seed path (first run,
	// or any scoped run). Filters cheap enough to push into SQL belong here;
	// everything else belongs in AcceptSeed.
	SeedQuery() store.SearchOptions

	// AcceptSeed is the per-fact seed predicate, applied on BOTH scan paths —
	// after SeedQuery on the full scan and after fact.ParseFact on the
	// incremental scan. Duplicating a SeedQuery filter here is correct and
	// intended: SQL cannot run on the incremental path, and a predicate that
	// exists on only one path makes the seed pool depend on watermark state.
	AcceptSeed(f fact.Fact) bool

	// Plan enqueues this tool's work items for a non-empty seed pool. It runs
	// exactly once, inside StartSession, after the session row exists. The
	// engine handles the empty-pool case (immediate completion) before
	// calling Plan, so implementations may assume len(seeds) > 0.
	Plan(ctx context.Context, d Deps, sess *store.PipelineSession, seeds []fact.Fact) error

	// OnPhaseAdvance runs for the winner of a phase CAS, and only for the
	// winner — so an insert made here happens at most once per session per
	// transition. A losing CAS is a benign no-op and never reaches this hook
	// (architecture/store/pipeline-session-phase-cas).
	OnPhaseAdvance(ctx context.Context, d Deps, sess *store.PipelineSession, from, to string) error

	// Decode parses and validates a response against its item. It MUST be
	// pure — no store access, no mutation — because the engine runs it BEFORE
	// claiming the item: any error it returns leaves the item unanswered and
	// fully retryable.
	//
	// normalized is the text actually persisted as the item's response, which
	// lets a strategy store a placeholder for an answer with no content. It
	// must be non-empty, since the claim CAS keys on `response IS NULL`.
	Decode(item *store.PipelineWorkItem, response string) (decision any, normalized string, err error)

	// Apply performs the mutations a decoded response calls for. It runs only
	// after the claim CAS was won, so it executes at most once per item. An
	// error here surfaces with the item already consumed — see
	// Pipeline.ContinueSessionForItem for why that is the safe direction.
	//
	// decision is exactly the value this strategy's own Decode returned.
	Apply(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem, decision any) error

	// Render builds the agent-facing view of an item. Pure presentation: the
	// engine decides whether to render or to advance the phase.
	Render(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem) (*WorkItemView, error)
}

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
	payload   DiscoverWorkPayload
	parsed    bool
	proposals []DiscoveredFact
}

// decodeDiscoverStep is the shared Decode half of the discover step. tool
// prefixes errors and logs so a mixed-tool log stays readable.
//
// A malformed *payload* IS fatal, unlike a malformed response: the payload is
// server-written, so failing to read it back means the queue is corrupt, not
// that the agent answered badly.
func decodeDiscoverStep(tool string, item *store.PipelineWorkItem, response string) (*discoverDecision, error) {
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
	return d, nil
}

// applyDiscoverStep is the shared Apply half of the discover step. Every
// failure below is logged and swallowed: a failure deep in the gate chain must
// not abort an in-progress session and lose its queued grounded work. Bad
// proposals are dropped, good ones land.
//
// The branch comes from sess.Branch, never from d.RI.AgentBranch().
func applyDiscoverStep(ctx context.Context, tool string, d Deps, sess *store.PipelineSession, dec *discoverDecision) error {
	if !dec.parsed {
		return nil
	}
	gates := DiscoveryGatesFor(d.RI, dec.payload.Direction)
	written, err := applyDiscoveredProposals(ctx, d.Facts, d.Search, d.RI.Embedder(),
		dec.payload, dec.proposals, gates, sess.Branch, d.RI.OntologyRoot(), d.OnProgress)
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
	return nil
}
