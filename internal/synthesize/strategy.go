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
	RI       *repos.RepoInstance
	Facts    store.FactIndex
	Search   SearchQuery
	Pipeline store.PipelineIndex
	Branches store.BranchIndex
	// Abstraction is the title-embedding axis and the restatement shortlist
	// built on it (the consolidation-scope fix). Review-pipeline only.
	Abstraction store.AbstractionIndex
	// Motifs is alias resolution over the motif vocabulary. Review-pipeline
	// only, like Abstraction: it is derived state rebuilt under review's own
	// budget, and no runtime path may pay for it.
	Motifs     store.MotifIndex
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

	// RenderPayload produces ONLY the payload a paged item ships beside its
	// prompt — byte-for-byte what Render would put in WorkItemView.Facts,
	// without building the prompt around it.
	//
	// It exists because pages after the first keep the facts and discard the
	// prompt, and building a prompt to throw it away is not free: review's
	// Render retrieves methodology once per fact in the item, so serving an
	// N-fact item across P pages cost N×P store queries where N suffices.
	//
	// MUST agree with Render byte-for-byte on the same item. The completion
	// token is derived from the served payload and validated against the
	// stored row, so a payload that differed between the two render paths
	// would make a multi-page item unanswerable. Cheap by contract: no store
	// access, no params.
	//
	// Returns "" for a step type that has no payload to ship beside its
	// prompt; the engine then falls back to a full Render.
	RenderPayload(item *store.PipelineWorkItem) (string, error)
}

// levelTriggeredStrategy is the optional half of Strategy for tools that own a
// pass whose trigger is CORPUS STATE rather than recent change.
//
// The engine's seed scan is edge-triggered: it diffs against a watermark and
// asks "what changed?". Some passes are level-triggered and ask "what is the
// corpus missing?" — review's motif backfill reads LiveFactsWithoutMotifs,
// which is corpus-wide and watermark-independent by design. Composing the two
// starved the second: an empty dirty set completed the session before the
// strategy ever planned, so on a quiet corpus the level-triggered pass could
// never run, and a corpus hydrated from a remote (facts present, no new
// commits) never backfilled at all (knomit#115).
//
// Optional rather than part of Strategy for the same reason as pagedStrategy:
// hypothesize has no level-triggered pass, and widening the required interface
// would force a meaningless implementation on every tool. The engine
// type-asserts; a strategy that does not implement this is simply never asked,
// and keeps the old behaviour exactly.
type levelTriggeredStrategy interface {
	// HasLevelTriggeredWork reports whether the corpus's own state gives this
	// strategy work to do, independent of what changed recently.
	//
	// It is consulted ONLY when the dirty seed pool is empty — the one moment
	// the edge-triggered gate would otherwise hide the answer. An error is
	// logged and treated as "no", so a strategy that cannot answer degrades to
	// the previous behaviour rather than failing the session.
	//
	// The implementation owns its own gating. Review's, in particular, must
	// return false below EffortMedium: backfill fires on any authored fact
	// lacking a motif, which is every fact on a motif-free corpus — precisely
	// the corpus MN5's test uses — so rescuing it at normal effort would change
	// what EffortNormal PRODUCES
	// (invariants/synthesize/motif/effort-amendment).
	HasLevelTriggeredWork(ctx context.Context, d Deps, branch string) (bool, error)
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
	// ClusterKey is the stored item's grouping key, carried to the wire so an
	// answering agent can tell WHAT it is holding (knomit#120).
	//
	// It matters because item types differ in what a NON-ANSWER costs. An
	// ordinary prune cluster is safe to no-op; a restatement pair (key prefixed
	// `restate-`) is not — a no-op records a DECLINED verdict against the
	// throttle and permanently retires the standing pair. Two 2-fact prune
	// items were identical on every other field, so the only safe fleet rule
	// was to treat every one as possibly destructive.
	//
	// The RAW key rather than a derived is_restatement flag: the key already
	// exists and is already computed, so it cannot drift from its source the
	// way a second declaration would. The cost is that consumers key off the
	// prefix, which is why the prefix is pinned as a wire contract by test.
	ClusterKey string
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
	// Health carries corpus-health descriptor lines recorded while planning the
	// session — coverage of the abstraction axis, the standing pair population
	// and where its top sits, what this session was funded to spend and why.
	//
	// Observability only: nothing in the engine reads it back, and no branch
	// anywhere depends on a value in it. It rides the session RESULT because
	// that is the one carrier that always reaches the calling agent — reflect
	// items exist only when a session found hypothesis transitions, and
	// progress events go to the server log.
	Health []string
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
	d.reinforcements = parsed.Reinforcements
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
	reinforced := applyReinforcements(ctx, d.Facts, d.Search, dec.payload,
		dec.reinforcements, sess.Branch, fact.ID12(d.RI.ID()), d.OnProgress)
	if len(reinforced) > 0 {
		// Deliberately NOT counted into ReviewStats. Reinforcement creates no
		// fact, so counting it as Synthesized would overstate what the session
		// added; and store.PipelineSessionStats has no column of its own for
		// it, so a new ReviewStats field would be dropped silently by
		// recordStats — recorded-looking and gone. It surfaces as a
		// detail-discover progress event per fact, and as this line.
		log.Info().Str("tool", tool).Str("session", sess.ID).
			Strs("facts", reinforced).Msg("discover: reinforced existing facts")
	}
	return nil
}
