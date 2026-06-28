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
// (normal — byte-identical to pre-discovery behaviour). Use
// NewReviewerWithEffort to opt into the medium/high discovery dial.
//
// ScopedCluster reaches the cache via store.SearchIndex.CachedClusterFacts on
// the per-repo Service; no separate cache parameter is threaded through the
// synthesize layer.
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

	// Store one distill work item if >1 seed (lower priority than prune).
	if len(seeds) > 1 {
		factsJSON, err := json.Marshal(seeds)
		if err != nil {
			return nil, fmt.Errorf("review: marshal seeds for distill: %w", err)
		}
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "distill",
			ClusterKey: "distill-all",
			FactsJSON:  string(factsJSON),
			Priority:   0.0,
		}
		if err := pipelineIdx.InsertPipelineWorkItem(ctx, item); err != nil {
			return nil, fmt.Errorf("review: insert distill item: %w", err)
		}
	}

	// Emergent-fact discovery: at effort >= medium, enqueue a "discover"
	// work item per bridge seed set so the agent can decide whether an
	// unstated forward consequence is entailed. Bridges come from the
	// scoped-cluster output we already have. Skipped at EffortNormal —
	// buildScoredBridges returns (nil, nil) there, which is the
	// byte-identical-prior regression contract (TestTask16_ForwardEffortNormal_ZeroDiscovers).
	cfg := QualityConfigFromRepo(r.ri)
	cr := store.ClusterResult{Clusters: map[int][]string{}}
	for i, c := range clusters {
		paths := make([]string, 0, len(c))
		for _, f := range c {
			paths = append(paths, f.File)
		}
		cr.Clusters[i] = paths
	}
	bridges, err := buildScoredBridges(ctx, idx, branch, seeds, cr, r.bridgeKind(), r.effort, cfg)
	if err != nil {
		return nil, fmt.Errorf("review: build bridges: %w", err)
	}
	for i, b := range bridges {
		payload := DiscoverWorkPayload{Direction: DiscoverForward, Bridge: b}
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
func (r *Reviewer) ContinueSession(ctx context.Context, sessionID, response string) (*ReviewResult, error) {
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
	// Use the session's recorded branch — the session was bound to that
	// branch at creation; do not reach into the live AgentBranch.
	branch := sess.Branch

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

	// Apply based on step type.
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
		if err := ApplyReflectDecisions(ctx, gs, idx, parsed, sess,
			r.ri.OntologyRoot(), reflectNoveltyThreshold(store.EmbedderThresholds(r.ri.Embedder()).ReflectNovelty), r.onProgress); err != nil {
			return nil, fmt.Errorf("review: apply reflect: %w", err)
		}

	case "prune":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts: %w", err)
		}
		inputPaths := make([]string, len(facts))
		for i, f := range facts {
			inputPaths[i] = f.File
		}
		result, err := parsePruneResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse prune response: %w", err)
		}
		if err := validatePrunePaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate prune: %w", err)
		}
		if _, err := ApplyPruneDecisions(ctx, gs, result.Decisions, result.Merges, "review", r.onProgress, branch, r.ri.OntologyRoot()); err != nil {
			return nil, fmt.Errorf("review: apply prune: %w", err)
		}

	case "distill":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, fmt.Errorf("review: unmarshal facts: %w", err)
		}
		inputPaths := make([]string, len(facts))
		for i, f := range facts {
			inputPaths[i] = f.File
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, fmt.Errorf("review: parse distill response: %w", err)
		}
		if err := validateDistillPaths(result, inputPaths); err != nil {
			return nil, fmt.Errorf("review: validate distill: %w", err)
		}
		_, writtenFacts, err := ApplyDistillDecisions(ctx, gs, result.Synthesize, result.Retract, "review", r.onProgress, branch, r.ri.OntologyRoot())
		if err != nil {
			return nil, fmt.Errorf("review: apply distill: %w", err)
		}

		// RAPTOR: if new facts were synthesized and we haven't hit max depth, enqueue deeper.
		const maxRaptorDepth = 3
		if len(writtenFacts) > 0 && item.Depth < maxRaptorDepth {
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
			raptorClusters, clErr := ScopedCluster(ctx, newFacts, idx, r.ri.ClusterResolution(), r.ri.ClusterMinCommunitySize(), r.onProgress, branch, "hypothesis")
			if clErr != nil {
				log.Warn().Err(clErr).Msg("review: RAPTOR clustering failed")
			} else {
				nextDepth := item.Depth + 1
				for ci, cluster := range raptorClusters {
					factsJSON, _ := json.Marshal(cluster)
					wItem := store.PipelineWorkItem{
						SessionID:  sessionID,
						StepType:   "distill",
						ClusterKey: fmt.Sprintf("raptor-d%d-c%d", nextDepth, ci),
						FactsJSON:  string(factsJSON),
						Priority:   float64(-nextDepth),
						Depth:      nextDepth,
					}
					if err := pipelineIdx.InsertPipelineWorkItem(ctx, wItem); err != nil {
						log.Warn().Err(err).Msg("review: RAPTOR enqueue failed")
					}
				}
				if len(raptorClusters) > 0 {
					log.Info().Int("depth", nextDepth).Int("clusters", len(raptorClusters)).Msg("review: RAPTOR enqueued deeper distill items")
				}
			}
		}

	case "discover":
		var payload DiscoverWorkPayload
		if err := json.Unmarshal([]byte(item.FactsJSON), &payload); err != nil {
			return nil, fmt.Errorf("review: unmarshal discover payload: %w", err)
		}
		// Discovery is non-fatal enrichment (matching the hypothesize
		// pipeline): a malformed proposal response — or a failure deep in the
		// gate chain — must not abort an in-progress review session and lose
		// its already-queued prune/distill work. Log and continue.
		parsed, perr := parseDiscoverResponse(response)
		if perr != nil {
			log.Warn().Err(perr).Str("session", sessionID).Msg("review: discover response parse failed; treating as no-op")
		} else {
			gates := r.discoveryGates(payload.Direction)
			if _, err := applyDiscoveredProposals(ctx, gs, idx, r.ri.Embedder(), payload, parsed.Proposals, gates, branch, r.ri.OntologyRoot(), r.onProgress); err != nil {
				log.Warn().Err(err).Str("session", sessionID).Msg("review: apply discover failed; continuing")
			}
		}

	default:
		return nil, fmt.Errorf("review: unknown step type %q", item.StepType)
	}

	// Mark item as answered.
	if err := pipelineIdx.SetPipelineWorkItemResponse(ctx, item.ID, response); err != nil {
		return nil, fmt.Errorf("review: set response: %w", err)
	}

	return r.nextItem(ctx, sess)
}

// discoveryGates resolves the verification gates for a discover step based on
// the direction. Forward (synthesis): confidence + dedup only. Backward
// (hypothesis): all three including BlastRadius. Thresholds come from the
// per-repo DiscoveryConfig accessors (Plan 03 Task 6); the dedup floor comes
// from the embedder's calibrated thresholds.
func (r *Reviewer) discoveryGates(dir DiscoverDirection) DiscoveryGates {
	g := DiscoveryGates{
		ConfidenceThreshold: r.ri.DiscoveryConfidenceThreshold(),
		DedupThreshold:      store.EmbedderThresholds(r.ri.Embedder()).Dedup,
	}
	if dir == DiscoverBackward {
		g.BlastRadiusThreshold = r.ri.DiscoveryBlastRadiusThreshold()
	}
	return g
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

	// No watermark → first run, all facts are dirty. Use the index (fast).
	//
	// Pragmatic facts (policies, heuristics) are excluded: the synthesis
	// pipeline merges and distills descriptive knowledge, and its output
	// path in decision.go does not carry Kind through mergedFact/distillFact.
	// Letting a pragmatic fact in would cause it to be silently rewritten as
	// epistemic on commit and the original deleted.
	if watermark == "" {
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

	log.Info().Str("session", sess.ID).Int("completed", completed).Msg("review: session complete")
	r.onProgress(ProgressEvent{Phase: "review-done", Message: fmt.Sprintf("session %s complete", sess.ID)})

	return &ReviewResult{
		SessionID: sess.ID,
		Done:      true,
		Summary:   &ReviewStats{},
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
