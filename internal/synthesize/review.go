// Package synthesize — Review orchestrator: multi-turn review sessions that
// connect clustering, prompt rendering, decision application, and session storage.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"knomit/internal/fact"
	"knomit/internal/llm"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
)

// Reviewer orchestrates multi-turn review sessions.
// Reviewer is a stateless dispatcher over a single MCP call. Session-scoped
// state — including whether the reflect step has been considered for a given
// session — lives on the pipeline_sessions row, not on this struct. The MCP
// handler constructs a fresh Reviewer per call; any per-session field on
// this struct would silently lose state between calls.
type Reviewer struct {
	ri         *repos.RepoInstance
	onProgress func(ProgressEvent)
	effort     Effort
	scope      ScopeFilter
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
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	return &Reviewer{
		ri:         ri,
		onProgress: onProgress,
		effort:     NormalizeEffort(effort),
		scope:      scope,
	}
}

// Effort returns the discovery dial this Reviewer was constructed with.
// Exposed so the MCP layer can log/expose the resolved effort back to clients.
func (r *Reviewer) Effort() Effort { return r.effort }

// storeIndices returns the store indices under the repo read lock.
func (r *Reviewer) storeIndices() (store.FactIndex, store.SearchIndex, store.PipelineIndex, store.BranchIndex) {
	var gs store.FactIndex
	var idx store.SearchIndex
	var pipelineIdx store.PipelineIndex
	var branches store.BranchIndex
	r.ri.WithRead(func(svc *store.Service) {
		gs = svc.Facts()
		idx = svc.Search()
		pipelineIdx = svc.Pipeline()
		branches = svc.Branches()
	})
	return gs, idx, pipelineIdx, branches
}

// StartSession creates a new review session, identifies dirty facts, clusters
// them, stores work items, and returns the first item to review.
//
// This is the boundary at which the agent branch is bound to the
// session: the value of ri.AgentBranch() at this moment becomes
// sess.Branch and travels with the session for the rest of its
// lifetime. All downstream Reviewer methods read sess.Branch — they
// never reach back into ri.AgentBranch() — so a session continuing
// across an AgentBranch change still operates on its original branch.
func (r *Reviewer) StartSession(ctx context.Context) (*ReviewResult, error) {
	totalStart := time.Now()
	gs, idx, pipelineIdx, _ := r.storeIndices()
	branch := r.ri.AgentBranch()

	sess, err := pipelineIdx.CreatePipelineSession(ctx, "review", branch)
	if err != nil {
		return nil, fmt.Errorf("review: create session: %w", err)
	}

	// Persist the scoped flag on the session row so completeSession can suppress
	// watermark advancement, even though the MCP handler reconstructs a fresh
	// Reviewer (with empty scope) on every continue call. Relying on the
	// in-memory r.scope would let the completing continue call — which carries no
	// domain/entities args — advance the watermark to HEAD and permanently hide
	// out-of-scope facts from future unscoped sessions. Fatal on error: silently
	// leaving Scoped=false reintroduces exactly that poisoning.
	if !r.scope.IsEmpty() {
		if err := pipelineIdx.MarkPipelineSessionScoped(ctx, sess.ID); err != nil {
			return nil, fmt.Errorf("review: mark session scoped: %w", err)
		}
		sess.Scoped = true
	}

	t := time.Now()
	seeds, err := r.dirtyFacts(ctx, branch, gs, idx, pipelineIdx)
	if err != nil {
		return nil, fmt.Errorf("review: dirty facts: %w", err)
	}
	log.Info().Str("session", sess.ID).Int("seeds", len(seeds)).Dur("elapsed", time.Since(t)).Msg("review: dirty facts")

	if len(seeds) == 0 {
		return r.completeSession(ctx, sess)
	}

	// Build scoped clusters.
	t = time.Now()
	clusters, err := ScopedCluster(ctx, seeds, idx, r.ri.ClusterResolution(), r.ri.ClusterMinCommunitySize(), r.onProgress, branch)
	if err != nil {
		return nil, fmt.Errorf("review: cluster: %w", err)
	}
	log.Info().Str("session", sess.ID).Int("clusters", len(clusters)).Dur("elapsed", time.Since(t)).Msg("review: clustering done")

	// Dedup pass: merge near-duplicates within each cluster before enqueueing.
	// The near-duplicate floor is model-dependent (see internal/retrieval).
	t = time.Now()
	dedupThreshold := store.EmbedderThresholds(r.ri.Embedder()).Dedup
	for i := range clusters {
		surviving, err := dedupCluster(ctx, clusters[i], gs, idx, dedupThreshold, "review", r.onProgress, branch)
		if err != nil {
			return nil, fmt.Errorf("review: dedup cluster %d: %w", i, err)
		}
		clusters[i] = surviving
	}
	log.Info().Str("session", sess.ID).Int("clusters", len(clusters)).Dur("elapsed", time.Since(t)).Msg("review: dedup done")
	// Filter to clusters with > 1 fact (nothing for LLM to reason about with just 1).
	var pruneClusters [][]factForLLM
	for _, c := range clusters {
		if len(c) > 1 {
			pruneClusters = append(pruneClusters, c)
		}
	}

	// Store prune work items — priority = cluster size (bigger = more urgent).
	//
	// Prune clusters are deliberately NOT chunked, unlike the distill items
	// below. Splitting a cluster across work items would silently forbid merges
	// across the chunk boundary — a change in dedup quality, not a transport
	// fix — and cluster size is already bounded by Louvain plus
	// ClusterMinCommunitySize. If mega-communities ever show up in real
	// corpora, the fix belongs at clustering resolution, not here.
	for i, cluster := range pruneClusters {
		factsJSON, err := json.Marshal(cluster)
		if err != nil {
			return nil, fmt.Errorf("review: marshal cluster %d: %w", i, err)
		}
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "prune",
			ClusterKey: fmt.Sprintf("cluster-%d", i),
			FactsJSON:  string(factsJSON),
			Priority:   float64(len(cluster)),
		}
		if err := pipelineIdx.InsertPipelineWorkItem(ctx, item); err != nil {
			return nil, fmt.Errorf("review: insert prune item: %w", err)
		}
	}

	// Store distill work items if >1 seed (lower priority than prune).
	//
	// The seed pool is unbounded — a first run scans the whole corpus
	// (dirtyFacts' full-scan path, Limit: 100_000) — so marshalling it into one
	// item put an entire knowledge base into a single prompt. Chunking splits it
	// into items whose payload fits maxDistillChunkBytes. Unlike prune, distill
	// loses nothing across a chunk boundary: it synthesizes upward from what it
	// is shown rather than reconciling facts against each other, and RAPTOR
	// follow-ups (enqueueRaptorFollowups) re-cluster the output of every chunk
	// together, so cross-chunk synthesis still happens one level up.
	//
	// Priority stays 0.0 for every chunk — the same band as before, below
	// prune's positive cluster-size priorities and above the negative
	// discover/reflect band (see forwardDiscoverPriority). Equal-priority chunks
	// are served in insertion order by NextPipelineWorkItem's `id ASC` tiebreak.
	if len(seeds) > 1 {
		for i, chunk := range chunkFacts(seeds, maxDistillChunkBytes) {
			factsJSON, err := json.Marshal(chunk)
			if err != nil {
				return nil, fmt.Errorf("review: marshal seeds for distill chunk %d: %w", i, err)
			}
			item := store.PipelineWorkItem{
				SessionID:  sess.ID,
				StepType:   "distill",
				ClusterKey: fmt.Sprintf("distill-all-%d", i),
				FactsJSON:  string(factsJSON),
				Priority:   0.0,
			}
			if err := pipelineIdx.InsertPipelineWorkItem(ctx, item); err != nil {
				return nil, fmt.Errorf("review: insert distill item %d: %w", i, err)
			}
		}
	}

	// Emergent-fact discovery: at effort >= medium, enqueue a "discover"
	// work item per bridge seed set so the agent can decide whether an
	// unstated forward consequence is entailed. Bridges come from the
	// scoped-cluster output we already have. Skipped at EffortNormal —
	// buildScoredBridges returns (nil, nil) there, which is the zero-discovery-
	// spend contract (TestTask16_ForwardEffortNormal_ZeroDiscovers).
	cfg := QualityConfigFromRepo(r.ri)
	cr := clusterResultFromGroups(clusters)
	// Dispatch: scoped sessions use the token-optional filtered generator;
	// unscoped sessions use the token-anchored scored generator. The scope is
	// empty in the unscoped case, so passing it to buildScoredBridges is a no-op.
	var bridges []BridgeSeedSet
	if !r.scope.IsEmpty() {
		bridges, err = buildFilteredBridges(ctx, idx, branch, seeds, cr, r.scope, r.effort, cfg)
	} else {
		bridges, err = buildScoredBridges(ctx, idx, branch, seeds, cr, r.bridgeKind(), r.effort, cfg, r.scope)
	}
	if err != nil {
		return nil, fmt.Errorf("review: build bridges: %w", err)
	}
	sl := scopeLabel(r.scope)
	for i, b := range bridges {
		payload := DiscoverWorkPayload{Direction: DiscoverForward, Bridge: b, ScopeLabel: sl}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("review: marshal discover payload %d: %w", i, err)
		}
		if err := pipelineIdx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "discover",
			ClusterKey: fmt.Sprintf("discover-fwd-%d", i),
			FactsJSON:  string(payloadJSON),
			Priority:   forwardDiscoverPriority(i),
		}); err != nil {
			return nil, fmt.Errorf("review: insert discover item %d: %w", i, err)
		}
	}

	log.Info().
		Str("session", sess.ID).
		Int("prune_clusters", len(pruneClusters)).
		Int("seeds", len(seeds)).
		Int("bridges", len(bridges)).
		Str("effort", string(r.effort)).
		Dur("total", time.Since(totalStart)).
		Msg("review: session started")
	r.onProgress(ProgressEvent{Phase: "review-start", Message: fmt.Sprintf("session %s: %d clusters, %d seeds, %d bridges", sess.ID, len(pruneClusters), len(seeds), len(bridges))})

	return r.nextItem(ctx, sess)
}

// bridgeKind returns the BridgeKind configured for this Reviewer, sourced
// from the per-repo DiscoveryConfig (Plan 03 Task 6).
func (r *Reviewer) bridgeKind() BridgeKind {
	return BridgeKindFromString(r.ri.DiscoveryBridge())
}

// reflectPriority is the fixed priority of the single "reflect" work item. It
// is the floor of the negative-priority band: forward "discover" items must
// stay strictly above it so they run before reflect. maxBridgeSeeds (bridge.go)
// caps the discover queue so the rank-derived priority can never reach it.
const reflectPriority = -100

// forwardDiscoverPriorityBase places forward "discover" work items just below
// the standard prune (priority = cluster size) and distill (priority 0) band,
// but above reflect (reflectPriority), so discovery stays low-priority
// enrichment that runs after the grounded maintenance work.
const forwardDiscoverPriorityBase = -10

// forwardDiscoverPriority ranks the i-th forward discover item. `bridges` is
// already sorted by Q descending, so rank == i preserves quality order
// among discover items while keeping every priority strictly negative.
//
// Crucially, priority is a function of RANK, not Q: feeding a score directly
// into the priority (the old `-10 + b.Strength` anti-pattern) let a
// high-score bridge exceed 0 and leapfrog the prune/distill items it must run
// after. This mirrors the backward (hypothesize) path's `-100 - i`, which was
// written to avoid the same "a large rank flips the priority positive" bug.
func forwardDiscoverPriority(rank int) float64 {
	return forwardDiscoverPriorityBase - float64(rank)
}

// ContinueSession processes the model's response for the current work item
// and returns the next item, or done if the session is complete.
//
// Equivalent to ContinueSessionForItem with itemID 0 (no item assertion).
// Kept as the plain form because most callers — RunAll, the tests, and any
// client that predates the item_id wire field — have no id to assert.
func (r *Reviewer) ContinueSession(ctx context.Context, sessionID, response string) (*ReviewResult, error) {
	return r.ContinueSessionForItem(ctx, sessionID, response, 0)
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
// The call is ordered peek → decode+validate → CAS-claim → apply:
//
//   - Decoding before claiming keeps the common failure class (malformed LLM
//     JSON, paths outside the item's inputs) fully retryable — the item is
//     left unanswered and the agent can try again.
//   - Claiming before applying is what makes a retry idempotent: the claim is
//     a CAS on `response IS NULL`, so a resubmitted response loses and its
//     mutations are skipped entirely. Applying first (the pre-fix order) let a
//     duplicate submission mint a second copy of the same synthesized facts.
//
// The deliberate tradeoff: a hard failure *during* apply loses that one item's
// decisions, because the item is already consumed and is not un-claimed. That
// is accepted — the corpus is left un-maintained rather than corrupted,
// whereas duplicate synthesis facts are corruption.
func (r *Reviewer) ContinueSessionForItem(ctx context.Context, sessionID, response string, itemID int64) (*ReviewResult, error) {
	gs, idx, pipelineIdx, _ := r.storeIndices()

	sess, err := pipelineIdx.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("review: session %q not found", sessionID)
	}
	if sess.Status != "active" {
		return nil, fmt.Errorf("review: session %q is %s, not active", sessionID, sess.Status)
	}

	// Get the current (unanswered) work item.
	item, err := pipelineIdx.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: next work item: %w", err)
	}
	if item == nil {
		// No unanswered items — let the dispatcher handle phase advancement
		// (work→reflect→done as appropriate). Don't short-circuit to
		// completeSession: that would skip the reflect phase entirely on
		// sessions where the last work item was just answered out-of-band.
		return r.nextItem(ctx, sess)
	}

	// D2 staleness guard. Rejecting outright — rather than silently answering
	// whatever is current — is the point: the response was reasoned about
	// against a different item's facts, so applying it here would validate it
	// against the wrong input paths.
	if itemID != 0 && itemID != item.ID {
		return nil, fmt.Errorf("review: response targets work item %d but item %d is current; "+
			"re-read the current item and answer that one", itemID, item.ID)
	}

	// Decode and validate first. Every error below this point and above the
	// claim leaves the item unanswered, so the agent can retry.
	dec, err := r.decodeItem(item, response)
	if err != nil {
		return nil, err
	}

	// Claim. Losing the CAS means this item was already answered — by a
	// concurrent caller, or by an earlier attempt of this very submission
	// whose response reached the DB. Its mutations are already applied, so
	// re-applying them here is exactly the duplication P0.4 exists to kill.
	claimed, err := pipelineIdx.AnswerPipelineWorkItem(ctx, item.ID, response)
	if err != nil {
		return nil, fmt.Errorf("review: answer work item: %w", err)
	}
	if !claimed {
		log.Info().Str("session", sessionID).Int64("item", item.ID).
			Msg("review: work item already answered; skipping apply")
		return r.nextItem(ctx, sess)
	}

	if err := r.applyItem(ctx, gs, idx, pipelineIdx, sess, item, dec); err != nil {
		return nil, err
	}

	return r.nextItem(ctx, sess)
}

// itemDecision is the decoded, validated product of a response for one work
// item — everything applyItem needs, and nothing that touches the store.
// ContinueSessionForItem treats it as opaque; it only shuttles it from the
// decode half to the apply half across the claim CAS.
//
// Exactly one field is populated, selected by the item's step type.
type itemDecision struct {
	reflect  *ReflectResult
	prune    *PruneResult
	distill  *DistillResult
	discover *discoverDecision
}

// discoverDecision is the discover step's decoded form.
//
// parsed is false when the response could not be parsed. That is deliberately
// not an error: discovery is non-fatal enrichment (matching the hypothesize
// pipeline), and aborting would kill an in-progress session and lose its
// already-queued prune/distill work. The item is still claimed and applied as
// a no-op — a response that failed to parse will not parse on a retry either,
// so leaving it unanswered would wedge the session on an unanswerable item.
type discoverDecision struct {
	payload   DiscoverWorkPayload
	parsed    bool
	proposals []DiscoveredFact
}

// decodeItem parses and validates a response against its work item. It is
// deliberately pure — no store access, no mutation — which is what lets
// ContinueSessionForItem run it before claiming the item: any error it
// returns leaves the item fully retryable.
func (r *Reviewer) decodeItem(item *store.PipelineWorkItem, response string) (*itemDecision, error) {
	switch item.StepType {
	case "reflect":
		var transitions []hypothesisTransition
		if err := json.Unmarshal([]byte(item.FactsJSON), &transitions); err != nil {
			return nil, fmt.Errorf("review: unmarshal transitions: %w", err)
		}
		transitionPaths := make([]string, len(transitions))
		for i, t := range transitions {
			transitionPaths[i] = t.Path
		}
		parsed, err := parseReflectResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse reflect response: %w", err)
		}
		if err := validateReflectResponse(parsed, transitionPaths, reflectProposeCap()); err != nil {
			return nil, fmt.Errorf("review: validate reflect: %w", err)
		}
		return &itemDecision{reflect: &parsed}, nil

	case "prune":
		inputPaths, err := itemInputPaths(item)
		if err != nil {
			return nil, err
		}
		result, err := parsePruneResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse prune response: %w", err)
		}
		if err := validatePrunePaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate prune: %w", err)
		}
		return &itemDecision{prune: &result}, nil

	case "distill":
		inputPaths, err := itemInputPaths(item)
		if err != nil {
			return nil, err
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse distill response: %w", err)
		}
		if err := validateDistillPaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate distill: %w", err)
		}
		return &itemDecision{distill: &result}, nil

	case "discover":
		var payload DiscoverWorkPayload
		if err := json.Unmarshal([]byte(item.FactsJSON), &payload); err != nil {
			return nil, fmt.Errorf("review: unmarshal discover payload: %w", err)
		}
		d := &discoverDecision{payload: payload}
		parsed, perr := parseDiscoverResponse(response)
		if perr != nil {
			log.Warn().Err(perr).Str("session", item.SessionID).Msg("review: discover response parse failed; treating as no-op")
		} else {
			d.parsed = true
			d.proposals = parsed.Proposals
		}
		return &itemDecision{discover: d}, nil

	default:
		return nil, fmt.Errorf("review: unknown step type %q", item.StepType)
	}
}

// itemInputPaths unmarshals a prune/distill item's fact payload and returns
// the paths the response is allowed to reference. Validating against these —
// not against the whole corpus — is what stops a response from acting on
// facts its item never showed the agent.
func itemInputPaths(item *store.PipelineWorkItem) ([]string, error) {
	var facts []factForLLM
	if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
		return nil, fmt.Errorf("review: unmarshal facts: %w", err)
	}
	paths := make([]string, len(facts))
	for i, f := range facts {
		paths[i] = f.File
	}
	return paths, nil
}

// applyItem performs the mutations a decoded response calls for. It runs only
// after the item's claim CAS was won, so it executes at most once per item.
// An error here surfaces to the caller with the item already consumed — see
// ContinueSessionForItem for why that tradeoff is the safe direction.
func (r *Reviewer) applyItem(
	ctx context.Context,
	gs store.FactIndex,
	idx store.SearchIndex,
	pipelineIdx store.PipelineIndex,
	sess *store.PipelineSession,
	item *store.PipelineWorkItem,
	dec *itemDecision,
) error {
	// Use the session's recorded branch — the session was bound to that
	// branch at creation; do not reach into the live AgentBranch.
	branch := sess.Branch

	switch item.StepType {
	case "reflect":
		if err := ApplyReflectDecisions(ctx, gs, idx, *dec.reflect, sess,
			r.ri.OntologyRoot(), reflectNoveltyThreshold(store.EmbedderThresholds(r.ri.Embedder()).ReflectNovelty), r.onProgress); err != nil {
			return fmt.Errorf("review: apply reflect: %w", err)
		}

	case "prune":
		stats, err := ApplyPruneDecisions(ctx, gs, dec.prune.Decisions, dec.prune.Merges, "review", r.onProgress, branch, r.ri.OntologyRoot())
		if err != nil {
			return fmt.Errorf("review: apply prune: %w", err)
		}
		r.recordStats(ctx, pipelineIdx, sess, stats)

	case "distill":
		stats, writtenFacts, err := ApplyDistillDecisions(ctx, gs, dec.distill.Synthesize, dec.distill.Retract, "review", r.onProgress, branch, r.ri.OntologyRoot())
		if err != nil {
			return fmt.Errorf("review: apply distill: %w", err)
		}
		r.recordStats(ctx, pipelineIdx, sess, stats)
		r.enqueueRaptorFollowups(ctx, idx, pipelineIdx, sess, item, writtenFacts)

	case "discover":
		// Non-fatal by design: a failure deep in the gate chain must not abort
		// an in-progress review session and lose its queued prune/distill work.
		if dec.discover.parsed {
			gates := r.discoveryGates(dec.discover.payload.Direction)
			if _, err := applyDiscoveredProposals(ctx, gs, idx, r.ri.Embedder(), dec.discover.payload, dec.discover.proposals, gates, branch, r.ri.OntologyRoot(), r.onProgress); err != nil {
				log.Warn().Err(err).Str("session", sess.ID).Msg("review: apply discover failed; continuing")
			}
		}

	default:
		return fmt.Errorf("review: unknown step type %q", item.StepType)
	}
	return nil
}

// recordStats accumulates one applied item's counts onto the session row, which
// is where the running totals have to live: the MCP handler builds a fresh
// Reviewer per call, so nothing kept on this struct survives to completion.
//
// A failure is logged and swallowed, and stats accumulated for an item whose
// apply then crashes are simply lost. Both are deliberate: the summary is
// informational, the mutations it describes are already committed, and D1
// already accepts losing a whole item's decisions to a mid-apply crash — so
// there is nothing here worth a transaction or a retry.
func (r *Reviewer) recordStats(ctx context.Context, pipelineIdx store.PipelineIndex, sess *store.PipelineSession, stats *ReviewStats) {
	if stats == nil {
		return
	}
	if err := pipelineIdx.AddPipelineSessionStats(ctx, sess.ID, store.PipelineSessionStats{
		Pruned:      stats.Pruned,
		Merged:      stats.Merged,
		Updated:     stats.Updated,
		Synthesized: stats.Synthesized,
	}); err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: could not record session stats")
	}
}

// maxRaptorDepth bounds RAPTOR recursion: each distill round can synthesize
// facts that seed another distill round, and without a ceiling a productive
// session would never drain its queue.
const maxRaptorDepth = 3

// enqueueRaptorFollowups clusters the facts a distill step just wrote and
// enqueues a deeper distill item per cluster, so synthesis can recurse over
// its own output. Every failure here is logged and swallowed: the distill
// decisions are already committed, and losing the follow-up round costs depth,
// not correctness.
func (r *Reviewer) enqueueRaptorFollowups(
	ctx context.Context,
	idx store.SearchIndex,
	pipelineIdx store.PipelineIndex,
	sess *store.PipelineSession,
	item *store.PipelineWorkItem,
	writtenFacts []distillFact,
) {
	if len(writtenFacts) == 0 || item.Depth >= maxRaptorDepth {
		return
	}

	// Build factForLLM from the written (normalized-path) facts for clustering.
	newFacts := make([]factForLLM, 0, len(writtenFacts))
	for _, df := range writtenFacts {
		newFacts = append(newFacts, factForLLM{
			File: df.Path, Title: df.Title, Body: df.Body,
			Type: df.Type, Domain: df.Domain, Entities: df.Entities,
			Confidence: df.Confidence, Sources: 1,
		})
	}

	// Cluster the new facts to find groups worth distilling further.
	raptorClusters, clErr := ScopedCluster(ctx, newFacts, idx, r.ri.ClusterResolution(), r.ri.ClusterMinCommunitySize(), r.onProgress, sess.Branch, "hypothesis")
	if clErr != nil {
		log.Warn().Err(clErr).Msg("review: RAPTOR clustering failed")
		return
	}

	nextDepth := item.Depth + 1
	for ci, cluster := range raptorClusters {
		// Same payload bound as the first-round distill items: a productive
		// distill can write enough facts that a single re-clustered group would
		// exceed one prompt. Priority stays -nextDepth for every chunk of a
		// given depth, so depth ordering is preserved and equal-priority chunks
		// fall back to NextPipelineWorkItem's `id ASC` tiebreak.
		for chi, chunk := range chunkFacts(cluster, maxDistillChunkBytes) {
			factsJSON, _ := json.Marshal(chunk)
			wItem := store.PipelineWorkItem{
				SessionID:  sess.ID,
				StepType:   "distill",
				ClusterKey: fmt.Sprintf("raptor-d%d-c%d-%d", nextDepth, ci, chi),
				FactsJSON:  string(factsJSON),
				Priority:   float64(-nextDepth),
				Depth:      nextDepth,
			}
			if err := pipelineIdx.InsertPipelineWorkItem(ctx, wItem); err != nil {
				log.Warn().Err(err).Msg("review: RAPTOR enqueue failed")
			}
		}
	}
	if len(raptorClusters) > 0 {
		log.Info().Int("depth", nextDepth).Int("clusters", len(raptorClusters)).Msg("review: RAPTOR enqueued deeper distill items")
	}
}

// discoveryGates resolves the verification gates for a discover step based on
// the direction, delegating to the package-level DiscoveryGatesFor so the gate
// set is defined once across the review and hypothesize paths.
func (r *Reviewer) discoveryGates(dir DiscoverDirection) DiscoveryGates {
	return DiscoveryGatesFor(r.ri, dir)
}

// RunAll drives the review session to completion using an LLM adapter.
// It starts a session, then loops: render prompt → LLM call → apply response
// until all work items are processed.
func (r *Reviewer) RunAll(ctx context.Context, adapter llm.LLMAdapter) error {
	result, err := r.StartSession(ctx)
	if err != nil {
		return fmt.Errorf("RunAll: start: %w", err)
	}
	if result.Done {
		return nil
	}

	sessionID := result.SessionID
	for result.Item != nil {
		r.onProgress(ProgressEvent{
			Phase:   "llm",
			Message: fmt.Sprintf("processing %s work item", result.Item.Type),
		})

		opts := llm.CompletionOptions{ForceJSON: true}
		response, err := adapter.Complete(ctx, "", []llm.Message{
			{Role: "user", Content: result.Item.Prompt},
		}, opts, nil)
		if err != nil {
			return fmt.Errorf("RunAll: LLM %s: %w", result.Item.Type, err)
		}

		result, err = r.ContinueSession(ctx, sessionID, response)
		if err != nil {
			return fmt.Errorf("RunAll: continue: %w", err)
		}
	}
	return nil
}

// dirtyFacts returns seed facts (changed since watermark).
//
// First run (no watermark): uses the search index to retrieve all facts
// without reading every file from git.
// Incremental (has watermark): uses DiffFiles to read only changed paths.
func (r *Reviewer) dirtyFacts(ctx context.Context, branch string, gs store.FactIndex, idx store.SearchIndex, pipelineIdx store.PipelineIndex) ([]factForLLM, error) {
	watermark, err := pipelineIdx.GetPipelineWatermark(ctx, "review", branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	// Full-scan path, taken when EITHER:
	//   - no watermark → first run, all facts are dirty; or
	//   - a scope filter is active → a scoped review is an on-demand pass over a
	//     slice of the corpus, independent of incremental change-tracking. Scoped
	//     sessions deliberately do NOT advance the watermark (see completeSession),
	//     so they must not be BLOCKED by it either: gating a scoped run on the
	//     shared watermark means that once a prior unscoped review pushed it to
	//     HEAD, every scoped review would diff an empty changeset and find zero
	//     seeds. Read and write sides must agree — scoped is exempt from both.
	//
	// Pragmatic facts (policies, heuristics) are excluded: the synthesis
	// pipeline merges and distills descriptive knowledge, and its output
	// path in decision.go does not carry Kind through mergedFact/distillFact.
	// Letting a pragmatic fact in would cause it to be silently rewritten as
	// epistemic on commit and the original deleted.
	if watermark == "" || !r.scope.IsEmpty() {
		// Scope is applied in Go via r.scope.Matches, NOT pushed into
		// SearchOptions: store.Search ANDs its domain+entity clauses
		// (intersection) and canonicalises domains, whereas the filter is union
		// with raw membership. Routing both first-run and incremental seeding
		// through Matches keeps one definition of scope membership, so the same
		// scope yields the same seed pool regardless of watermark.
		results, err := idx.Search(ctx, branch, store.SearchOptions{
			Limit:        100_000,
			IncludeKinds: []string{string(fact.Epistemic)},
		})
		if err != nil {
			return nil, fmt.Errorf("search all: %w", err)
		}
		facts := make([]factForLLM, 0, len(results))
		for _, sr := range results {
			if !r.scope.Matches(sr.Domain, sr.Entities) {
				continue
			}
			facts = append(facts, factForLLM{
				File:       sr.Path,
				Title:      sr.Title,
				Body:       sr.Body,
				Type:       sr.Type,
				Domain:     sr.Domain,
				Entities:   sr.Entities,
				Confidence: sr.Confidence,
				Sources:    sr.Sources,
				Origin:     sr.Origin,
			})
		}
		return facts, nil
	}

	// Incremental: only changed facts since watermark.
	added, modified, _, err := gs.DiffFiles(ctx, branch, watermark)
	if err != nil {
		return nil, fmt.Errorf("diff files: %w", err)
	}

	var seeds []factForLLM
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
		if f.Kind != fact.Epistemic {
			continue // synthesis does not operate on pragmatic facts (see comment above)
		}
		if !r.scope.Matches(f.Domain, f.Entities) {
			continue
		}
		seeds = append(seeds, factForLLM{
			File:       f.Path(),
			Title:      f.Title,
			Body:       f.Body,
			Type:       string(f.Type),
			Domain:     f.Domain,
			Entities:   f.Entities,
			Confidence: f.Confidence,
			Sources:    f.Sources,
			Origin:     string(f.Origin),
		})
	}
	return seeds, nil
}

// nextItem dispatches based on the session's persistent phase. It is
// intentionally short — all session-scoped state (including "have we
// considered enqueueing reflect for this session?") lives on the
// pipeline_sessions row, not on this Reviewer instance, because the MCP
// handler constructs a fresh Reviewer per call.
//
// sess.Branch is the branch this session was created against; it does not
// change as the user's live AgentBranch changes during the session.
func (r *Reviewer) nextItem(ctx context.Context, sess *store.PipelineSession) (*ReviewResult, error) {
	switch sess.Phase {
	case "work":
		return r.handleWorkPhase(ctx, sess)
	case "reflect":
		return r.handleReflectPhase(ctx, sess)
	case "done":
		return r.completeSession(ctx, sess)
	default:
		return nil, fmt.Errorf("review: unknown phase %q on session %s", sess.Phase, sess.ID)
	}
}

// handleWorkPhase serves the next prune/distill item if one remains; once
// the queue is empty, advances the session to phase=reflect and (if any
// hypothesis transitions occurred) enqueues exactly one reflect item.
//
// The work→reflect advance is a CAS on the phase column, so concurrent
// continuations can't both enqueue: at most one caller's UPDATE matches the
// "work" guard. The other observes the row already advanced and falls
// through to the reflect-phase dispatch on the next iteration.
func (r *Reviewer) handleWorkPhase(ctx context.Context, sess *store.PipelineSession) (*ReviewResult, error) {
	_, _, pipelineIdx, _ := r.storeIndices()

	item, err := pipelineIdx.NextPipelineWorkItem(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("review: next item: %w", err)
	}
	if item != nil {
		return r.renderWorkItem(ctx, sess, item)
	}

	advanced, err := pipelineIdx.AdvancePipelineSessionPhase(ctx, sess.ID, "work", "reflect")
	if err != nil {
		return nil, fmt.Errorf("review: advance work→reflect: %w", err)
	}
	if advanced {
		log.Info().Str("session", sess.ID).Str("from", "work").Str("to", "reflect").Msg("review: phase transition")
		if err := r.maybeEnqueueReflectItem(ctx, sess); err != nil {
			return nil, err
		}
	}
	return r.refetchAndDispatch(ctx, sess.ID)
}

// handleReflectPhase serves the (single) reflect work item if one is
// pending, otherwise advances reflect→done and dispatches into completion.
func (r *Reviewer) handleReflectPhase(ctx context.Context, sess *store.PipelineSession) (*ReviewResult, error) {
	_, _, pipelineIdx, _ := r.storeIndices()

	item, err := pipelineIdx.NextPipelineWorkItem(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("review: next item: %w", err)
	}
	if item != nil {
		return r.renderWorkItem(ctx, sess, item)
	}

	advanced, err := pipelineIdx.AdvancePipelineSessionPhase(ctx, sess.ID, "reflect", "done")
	if err != nil {
		return nil, fmt.Errorf("review: advance reflect→done: %w", err)
	}
	if advanced {
		log.Info().Str("session", sess.ID).Str("from", "reflect").Str("to", "done").Msg("review: phase transition")
	}
	return r.refetchAndDispatch(ctx, sess.ID)
}

// maybeEnqueueReflectItem inserts a single reflect work item iff there are
// hypothesis transitions to reflect on. Called only from the winner of the
// work→reflect CAS, which guarantees at-most-once insertion. A failure to
// detect transitions is logged but not fatal — the session still advances
// (matching the pre-fix tolerance).
func (r *Reviewer) maybeEnqueueReflectItem(ctx context.Context, sess *store.PipelineSession) error {
	_, _, pipelineIdx, _ := r.storeIndices()

	transitions, err := r.findHypothesisTransitions(ctx, sess)
	if err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: failed to find hypothesis transitions")
		return nil
	}
	if len(transitions) == 0 {
		return nil
	}
	transJSON, err := json.Marshal(transitions)
	if err != nil {
		return fmt.Errorf("review: marshal transitions: %w", err)
	}
	return pipelineIdx.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "reflect",
		ClusterKey: "reflect",
		FactsJSON:  string(transJSON),
		Priority:   reflectPriority,
	})
}

// refetchAndDispatch reloads the session row and re-enters nextItem so the
// dispatcher sees the post-advance phase. Used after a phase transition or
// when the in-memory phase value may be stale.
func (r *Reviewer) refetchAndDispatch(ctx context.Context, sessionID string) (*ReviewResult, error) {
	_, _, pipelineIdx, _ := r.storeIndices()
	fresh, err := pipelineIdx.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("review: refetch session: %w", err)
	}
	if fresh == nil {
		return nil, fmt.Errorf("review: session %q disappeared mid-dispatch", sessionID)
	}
	return r.nextItem(ctx, fresh)
}

// renderWorkItem builds a ReviewResult for the given work item — prompt,
// schema, progress counts. Pure rendering; the dispatcher decides whether
// to call this or advance the phase.
func (r *Reviewer) renderWorkItem(ctx context.Context, sess *store.PipelineSession, item *store.PipelineWorkItem) (*ReviewResult, error) {
	_, _, pipelineIdx, _ := r.storeIndices()
	branch := sess.Branch
	ontologyRoot := r.ri.OntologyRoot()

	var content *WorkItemContent
	var err error
	switch item.StepType {
	case "prune":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts for prompt: %w", err)
		}
		content, err = RenderPruneWorkItem(facts, ontologyRoot)
	case "distill":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts for prompt: %w", err)
		}
		applicableMethodology := r.loadDistillMethodology(ctx, branch, facts)
		content, err = RenderDistillWorkItem(facts, ontologyRoot, applicableMethodology)
	case "reflect":
		existingMethodology := r.loadReflectMethodology(ctx, branch, []byte(item.FactsJSON))
		content, err = RenderReflectWorkItem([]byte(item.FactsJSON), ontologyRoot, existingMethodology)
	case "discover":
		var payload DiscoverWorkPayload
		if uerr := json.Unmarshal([]byte(item.FactsJSON), &payload); uerr != nil {
			return nil, fmt.Errorf("review: unmarshal discover payload for prompt: %w", uerr)
		}
		content = RenderDiscoverWorkItem(payload, ontologyRoot)
	default:
		return nil, fmt.Errorf("review: unknown step type %q", item.StepType)
	}
	if err != nil {
		return nil, fmt.Errorf("review: render %s prompt: %w", item.StepType, err)
	}

	completed, remaining, err := pipelineIdx.PipelineWorkItemStats(ctx, sess.ID)
	if err != nil {
		return nil, fmt.Errorf("review: work item stats: %w", err)
	}

	return &ReviewResult{
		SessionID: sess.ID,
		Item: &ReviewItem{
			ID:             item.ID,
			Type:           item.StepType,
			Prompt:         content.Prompt,
			ResponseSchema: content.ResponseSchema,
		},
		Progress: &ReviewProgress{
			Completed: completed,
			Remaining: remaining,
		},
	}, nil
}

// completeSession marks the session done and advances the watermark.
// Branch comes from sess.Branch — the recorded branch the session was
// created against — so HEAD lookup and watermark advance are consistent.
func (r *Reviewer) completeSession(ctx context.Context, sess *store.PipelineSession) (*ReviewResult, error) {
	_, _, pipelineIdx, branches := r.storeIndices()
	branch := sess.Branch

	if err := pipelineIdx.CompletePipelineSession(ctx, sess.ID); err != nil {
		return nil, fmt.Errorf("review: complete session: %w", err)
	}

	// A scoped review only processed a subset of facts. Advancing the watermark
	// to HEAD would permanently hide facts outside the scope from future
	// unscoped sessions. Read the scoped flag off the session row (persisted in
	// StartSession) rather than r.scope: the MCP handler rebuilds the Reviewer
	// with empty scope on the completing continue call, so r.scope is unreliable
	// here.
	if !sess.Scoped {
		headHash, err := branches.HeadCommit(ctx, branch)
		if err != nil {
			log.Warn().Err(err).Msg("review: could not get HEAD for watermark")
		} else {
			if err := pipelineIdx.SetPipelineWatermark(ctx, "review", branch, headHash); err != nil {
				log.Warn().Err(err).Msg("review: could not advance watermark")
			}
		}
	}

	completed, _, err := pipelineIdx.PipelineWorkItemStats(ctx, sess.ID)
	if err != nil {
		log.Warn().Err(err).Msg("review: could not get final stats")
	}

	// Re-read the row rather than trusting the `sess` we were handed: it was
	// fetched before this call's item was applied, so its counters are one item
	// stale. A failure here costs only the summary numbers, so it degrades to a
	// zero summary — the same thing clients got before stats existed.
	summary := &ReviewStats{}
	if fresh, err := pipelineIdx.GetPipelineSession(ctx, sess.ID); err != nil {
		log.Warn().Err(err).Msg("review: could not read session stats for summary")
	} else if fresh != nil {
		summary = &ReviewStats{
			Pruned:      fresh.Stats.Pruned,
			Merged:      fresh.Stats.Merged,
			Updated:     fresh.Stats.Updated,
			Synthesized: fresh.Stats.Synthesized,
		}
	}

	log.Info().Str("session", sess.ID).Int("completed", completed).Msg("review: session complete")
	r.onProgress(ProgressEvent{Phase: "review-done", Message: fmt.Sprintf("session %s complete", sess.ID)})

	return &ReviewResult{
		SessionID: sess.ID,
		Done:      true,
		Summary:   summary,
		Progress:  &ReviewProgress{Completed: completed, Remaining: 0},
	}, nil
}

// methodologyTopK is the per-fact retrieval depth and the final merged-list
// cap; both share one knob since callers want "show up to N methodologies"
// in the prompt regardless of cluster size.
const methodologyTopK = 3

// loadReflectMethodology retrieves methodology relevant to each transitioned
// hypothesis fact independently, then merges the per-fact results (keeping
// the highest score per methodology path). Returns "" on any failure (logged)
// or when no methodology matches.
//
// branch is required (no implicit AgentBranch fallback): callers pass the
// session's recorded branch so retrieval lands on the same branch the
// session was created against.
func (r *Reviewer) loadReflectMethodology(ctx context.Context, branch string, transitionsJSON []byte) string {
	var ts []hypothesisTransition
	if err := json.Unmarshal(transitionsJSON, &ts); err != nil {
		log.Warn().Err(err).Msg("loadReflectMethodology: transitions JSON malformed; skipping methodology section")
		return ""
	}
	if len(ts) == 0 {
		return ""
	}

	var merged []store.MethodologyMatch
	r.ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			log.Error().Str("branch", branch).Msg("loadReflectMethodology: nil store service; methodology disabled")
			return
		}
		minScore := r.ri.MethodologyMinScore()
		seen := map[string]store.MethodologyMatch{}
		for _, t := range ts {
			if err := ctx.Err(); err != nil {
				log.Warn().Err(err).Str("branch", branch).Str("path", t.Path).
					Msg("loadReflectMethodology: ctx canceled mid-iteration")
				return
			}
			f, err := svc.Search().GetByPath(ctx, branch, t.Path)
			if err != nil {
				log.Warn().Err(err).Str("branch", branch).Str("path", t.Path).
					Msg("loadReflectMethodology: transition fact lookup failed; skipping")
				continue
			}
			if f == nil {
				continue
			}
			matches, mErr := svc.Search().RelevantMethodologyForFact(
				ctx, branch, f.Path, f.Domain, f.Entities, methodologyTopK, minScore,
			)
			if mErr != nil {
				log.Warn().Err(mErr).Str("branch", branch).Str("path", f.Path).
					Msg("loadReflectMethodology: per-fact retrieval failed; skipping")
				continue
			}
			for _, m := range matches {
				if existing, ok := seen[m.Path]; !ok || m.Score > existing.Score {
					seen[m.Path] = m
				}
			}
		}
		merged = topKByScore(seen, methodologyTopK)
	})
	return store.FormatMethodologySection(merged)
}

// loadDistillMethodology retrieves methodology relevant to each input fact
// independently, then merges the per-fact results. Returns "" on failure
// (logged) or when no methodology matches.
//
// branch is required (no implicit AgentBranch fallback).
func (r *Reviewer) loadDistillMethodology(ctx context.Context, branch string, facts []factForLLM) string {
	if len(facts) == 0 {
		return ""
	}

	var merged []store.MethodologyMatch
	r.ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			log.Error().Str("branch", branch).Msg("loadDistillMethodology: nil store service; methodology disabled")
			return
		}
		minScore := r.ri.MethodologyMinScore()
		seen := map[string]store.MethodologyMatch{}
		for _, f := range facts {
			if err := ctx.Err(); err != nil {
				log.Warn().Err(err).Str("branch", branch).Str("path", f.File).
					Msg("loadDistillMethodology: ctx canceled mid-iteration")
				return
			}
			matches, mErr := svc.Search().RelevantMethodologyForFact(
				ctx, branch, f.File, f.Domain, f.Entities, methodologyTopK, minScore,
			)
			if mErr != nil {
				log.Warn().Err(mErr).Str("branch", branch).Str("path", f.File).
					Msg("loadDistillMethodology: per-fact retrieval failed; skipping")
				continue
			}
			for _, m := range matches {
				if existing, ok := seen[m.Path]; !ok || m.Score > existing.Score {
					seen[m.Path] = m
				}
			}
		}
		merged = topKByScore(seen, methodologyTopK)
	})
	return store.FormatMethodologySection(merged)
}

// topKByScore returns the up-to-k MethodologyMatch values from seen,
// sorted descending by Score. SQL row order is not preserved across the
// map iteration; ties are broken by Path for determinism.
func topKByScore(seen map[string]store.MethodologyMatch, k int) []store.MethodologyMatch {
	if len(seen) == 0 {
		return nil
	}
	out := make([]store.MethodologyMatch, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// hypothesisTransition records a change to a hypothesis fact during a review session.
type hypothesisTransition struct {
	Path         string `json:"path"`
	OriginalType string `json:"original_type"`
	Action       string `json:"action"` // "promoted", "retracted", "confidence-updated"
	Detail       string `json:"detail,omitempty"`
}

// findHypothesisTransitions detects hypothesis facts that were promoted,
// retracted, or had their confidence changed during the given review
// session. All branch reads use sess.Branch — the session's recorded
// branch — so the diff is consistent with the session's scope.
func (r *Reviewer) findHypothesisTransitions(ctx context.Context, sess *store.PipelineSession) ([]hypothesisTransition, error) {
	gs, _, pipelineIdx, _ := r.storeIndices()
	branch := sess.Branch

	// Read the watermark set at the end of the previous session.
	// Since we haven't advanced it yet (that happens in completeSession),
	// all commits between here and HEAD are changes made during this session.
	watermark, err := pipelineIdx.GetPipelineWatermark(ctx, "review", branch)
	if err != nil || watermark == "" {
		return nil, err
	}

	_, modified, deleted, err := gs.DiffFiles(ctx, branch, watermark)
	if err != nil {
		return nil, err
	}

	var transitions []hypothesisTransition

	// Check deleted paths — were any hypotheses retracted?
	for _, path := range deleted {
		readResult, err := gs.ReadFact(ctx, branch, path, &store.ReadFactOpts{AtCommit: watermark})
		if err != nil {
			continue
		}
		f, err := fact.ParseFact(path, readResult.Content)
		if err != nil {
			continue
		}
		if f.Type == fact.Hypothesis {
			transitions = append(transitions, hypothesisTransition{
				Path: path, OriginalType: "hypothesis", Action: "retracted",
			})
		}
	}

	// Check modified paths — did any hypothesis change confidence or type?
	for _, path := range modified {
		oldResult, err := gs.ReadFact(ctx, branch, path, &store.ReadFactOpts{AtCommit: watermark})
		if err != nil {
			continue
		}
		oldFact, err := fact.ParseFact(path, oldResult.Content)
		if err != nil || oldFact.Type != fact.Hypothesis {
			continue
		}
		newResult, err := gs.ReadFact(ctx, branch, path, nil)
		if err != nil {
			continue
		}
		newFact, err := fact.ParseFact(path, newResult.Content)
		if err != nil {
			continue
		}
		if newFact.Type != oldFact.Type {
			transitions = append(transitions, hypothesisTransition{
				Path: path, OriginalType: "hypothesis", Action: "promoted",
				Detail: fmt.Sprintf("type changed to %s", newFact.Type),
			})
		} else if newFact.Confidence != oldFact.Confidence {
			transitions = append(transitions, hypothesisTransition{
				Path: path, OriginalType: "hypothesis", Action: "confidence-updated",
				Detail: fmt.Sprintf("%.2f → %.2f", oldFact.Confidence, newFact.Confidence),
			})
		}
	}

	return transitions, nil
}
