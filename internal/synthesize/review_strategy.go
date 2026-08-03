// Package synthesize — reviewStrategy: the review tool's half of the engine.
//
// Everything here is what makes a review session a *review* session rather
// than some other synthesis pass: epistemic-only seeding, the
// prune/distill/discover work plan, the reflect item, and the decode/apply/
// render behaviour of each step type. The session lifecycle, phase machine,
// seed scan, claim protocol, and completion all live in pipeline.go.
//
// reviewStrategy is a zero-size struct on purpose. It is rebuilt per MCP call
// alongside its Pipeline, so a field here would be as invisible across turns
// as a field on the engine (invariants/synthesize/
// per-call-objects-no-session-state). Everything it needs arrives as Deps or
// off the session row.
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
)

// reviewStrategy implements Strategy for the corpus-maintenance (review)
// pipeline: dedup/prune, distill, optional discovery, and reflect.
type reviewStrategy struct{}

// reviewTool is the pipeline_sessions.tool value and watermark key. It is also
// the recipe name threaded into decision application, so the two cannot drift.
const reviewTool = "review"

func (reviewStrategy) Tool() string { return reviewTool }

// SeedQuery scans the whole corpus, epistemic facts only.
//
// The kind restriction is pushed into SQL here purely as an efficiency measure;
// AcceptSeed repeats it, and AcceptSeed is the authoritative statement of the
// rule (see conventions/synthesize/dirty-facts-excludes-pragmatic).
func (reviewStrategy) SeedQuery() store.SearchOptions {
	return store.SearchOptions{
		Limit:        100_000,
		IncludeKinds: []string{string(fact.Epistemic)},
	}
}

// AcceptSeed keeps pragmatic facts (policies, heuristics) out of synthesis.
//
// This is a correctness filter, not a preference: the synthesis output path in
// decision.go does not carry Kind through mergedFact/distillFact, so a
// pragmatic fact that reached synthesis would be silently rewritten as
// epistemic on commit and the original deleted. It runs on BOTH scan paths —
// the search hit's kind column on a full scan, fact.ParseFact's kind on the
// incremental scan — because a filter present on only one path would make the
// seed pool depend on watermark state.
func (reviewStrategy) AcceptSeed(f fact.Fact) bool {
	return f.Kind == fact.Epistemic
}

// Plan builds the review work queue over a non-empty seed pool: cluster, dedup,
// then enqueue prune items per surviving multi-fact cluster, distill items over
// the (chunked) seed pool, and — at effort >= medium — discover items per
// bridge seed set.
//
// branch comes off sess, never off d.RI: by the time Plan runs, the session is
// already bound to the branch it was created against.
func (reviewStrategy) Plan(ctx context.Context, d Deps, sess *store.PipelineSession, seeds []fact.Fact) error {
	branch := sess.Branch
	llmSeeds := factsForLLM(seeds)

	// Build scoped clusters.
	t := time.Now()
	clusters, err := ScopedCluster(ctx, llmSeeds, d.Search, d.RI.ClusterResolution(), d.RI.ClusterMinCommunitySize(), d.OnProgress, branch)
	if err != nil {
		return wrapf(reviewTool, err, "cluster")
	}
	log.Info().Str("session", sess.ID).Int("clusters", len(clusters)).Dur("elapsed", time.Since(t)).Msg("review: clustering done")

	// Dedup pass: merge near-duplicates within each cluster before enqueueing.
	// The near-duplicate floor is model-dependent (see internal/embeddings/params).
	t = time.Now()
	dedupThreshold := store.EmbedderThresholds(d.RI.Embedder()).Dedup
	for i := range clusters {
		surviving, err := dedupCluster(ctx, clusters[i], d.Facts, d.Search, dedupThreshold, reviewTool, d.OnProgress, branch, fact.ID12(d.RI.ID()))
		if err != nil {
			return wrapf(reviewTool, err, "dedup cluster %d", i)
		}
		clusters[i] = surviving
	}
	log.Info().Str("session", sess.ID).Int("clusters", len(clusters)).Dur("elapsed", time.Since(t)).Msg("review: dedup done")

	// Filter to clusters with > 1 fact (nothing for the LLM to reason about
	// with just one).
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
			return wrapf(reviewTool, err, "marshal cluster %d", i)
		}
		// Prune stays unchunked, so nothing bounds a cluster except what Louvain
		// happened to produce. Paging makes an oversized one DELIVERABLE — it
		// arrives across pages — but the agent still has to hold all of it to
		// decide merges, so a mega-community can exceed what the model can
		// reason over even though every page fits. Say so rather than letting a
		// degraded merge pass silently: the whole failure class this area keeps
		// hitting is caps that nothing reports.
		if len(factsJSON) > maxItemBytes {
			log.Warn().Str("session", sess.ID).Str("cluster", fmt.Sprintf("cluster-%d", i)).
				Int("facts", len(cluster)).Int("bytes", len(factsJSON)).Int("limit", maxItemBytes).
				Msg("review: prune cluster exceeds maxItemBytes; it will page, but merge quality may suffer — consider raising clustering resolution")
		}
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "prune",
			ClusterKey: fmt.Sprintf("cluster-%d", i),
			FactsJSON:  string(factsJSON),
			Priority:   float64(len(cluster)),
		}
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, item); err != nil {
			return wrapf(reviewTool, err, "insert prune item")
		}
	}

	// Store distill work items if >1 seed (lower priority than prune).
	//
	// Grouped by cluster, then chunked. Depth-0 distill used to pass the whole
	// flat seed pool to chunkFacts, so an item was an arbitrary byte slice of
	// the corpus in scan order — prune and the RAPTOR follow-ups both worked on
	// real communities and depth-0 was the odd one out. Grouping first is what
	// makes a bounded item stop being a quality loss: the bound comes from the
	// corpus's own structure rather than from a byte count that happens to land
	// mid-topic.
	//
	// chunkFacts still runs, now as a backstop rather than the primary
	// mechanism: a single Louvain community can still exceed one payload, and
	// until work-item paging exists this is the only thing standing between a
	// first run (dirtyFacts' full-scan path, Limit: 100_000) and a prompt
	// containing an entire knowledge base. Splitting a group loses less than it
	// looks — distill synthesizes upward from what it is shown rather than
	// reconciling facts against each other, and enqueueRaptorFollowups
	// re-clusters each round's output, so cross-chunk synthesis happens one
	// level up.
	//
	// Priority stays 0.0 for every item — the same band as before, below
	// prune's positive cluster-size priorities and above the negative
	// discover/reflect band (see forwardDiscoverPriority). Equal-priority items
	// are served in insertion order by NextPipelineWorkItem's `id ASC` tiebreak.
	if len(llmSeeds) > 1 {
		for _, group := range distillGroups(llmSeeds, clusters) {
			for ci, chunk := range chunkFacts(group.Facts, maxItemBytes) {
				factsJSON, err := json.Marshal(chunk)
				if err != nil {
					return wrapf(reviewTool, err, "marshal seeds for distill group %s chunk %d", group.Key, ci)
				}
				item := store.PipelineWorkItem{
					SessionID:  sess.ID,
					StepType:   "distill",
					ClusterKey: fmt.Sprintf("%s-%d", group.Key, ci),
					FactsJSON:  string(factsJSON),
					Priority:   0.0,
				}
				if err := d.Pipeline.InsertPipelineWorkItem(ctx, item); err != nil {
					return wrapf(reviewTool, err, "insert distill item %s-%d", group.Key, ci)
				}
			}
		}
	}

	// Emergent-fact discovery: at effort >= medium, enqueue a "discover"
	// work item per bridge seed set so the agent can decide whether an
	// unstated forward consequence is entailed. Bridges come from the
	// scoped-cluster output we already have. Skipped at EffortNormal —
	// the bridge builders return (nil, nil) there, which is the
	// zero-discovery-spend contract of
	// invariants/synthesize/effort-normal-byte-identical.
	cfg := QualityConfigFromRepo(d.RI)
	cr := clusterResultFromGroups(clusters)
	// Dispatch: scoped sessions use the token-optional filtered generator;
	// unscoped sessions use the token-anchored scored generator. The scope is
	// empty in the unscoped case, so passing it to buildScoredBridges is a no-op.
	var bridges []BridgeSeedSet
	if !d.Scope.IsEmpty() {
		bridges, err = buildFilteredBridges(ctx, d.Search, branch, llmSeeds, cr, d.Scope, d.Effort, cfg)
	} else {
		bridges, err = buildScoredBridges(ctx, d.Search, branch, llmSeeds, cr, BridgeKindFromString(d.RI.DiscoveryBridge()), d.Effort, cfg, d.Scope)
	}
	if err != nil {
		return wrapf(reviewTool, err, "build bridges")
	}
	sl := scopeLabel(d.Scope)
	for i, b := range bridges {
		payload := DiscoverWorkPayload{Direction: DiscoverForward, Bridge: b, ScopeLabel: sl}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return wrapf(reviewTool, err, "marshal discover payload %d", i)
		}
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "discover",
			ClusterKey: fmt.Sprintf("discover-fwd-%d", i),
			FactsJSON:  string(payloadJSON),
			Priority:   forwardDiscoverPriority(i),
		}); err != nil {
			return wrapf(reviewTool, err, "insert discover item %d", i)
		}
	}

	log.Info().
		Str("session", sess.ID).
		Int("prune_clusters", len(pruneClusters)).
		Int("seeds", len(llmSeeds)).
		Int("bridges", len(bridges)).
		Str("effort", string(d.Effort)).
		Msg("review: work planned")
	d.OnProgress(ProgressEvent{Phase: "review-start", Message: fmt.Sprintf("session %s: %d clusters, %d seeds, %d bridges", sess.ID, len(pruneClusters), len(llmSeeds), len(bridges))})

	return nil
}

// pagedStepTypes are the review steps whose payload ships BESIDE the prompt and
// can therefore be delivered across pages.
//
// Explicit rather than inferred, because the two sides of paging read different
// fields and only agree for these steps. reviewResultPage pages item.Facts —
// what Render chose to ship beside the prompt — while RequireCompletion must
// work from item.FactsJSON, the stored row. reflect and discover interpolate
// their payloads into the prompt, so they never page and are never issued a
// token; but their stored JSON is still fact-shaped enough to slice (a
// hypothesisTransition even has a "path" field, which unmarshals onto
// factForLLM.File). Inferring "can page" from the stored payload would demand a
// token that was never issued and leave the item permanently unanswerable.
//
// Keep in sync with Render. TestPaging_TokenRequiredOnlyWhereItIsIssued pins it.
var pagedStepTypes = map[string]bool{
	"distill": true,
	"prune":   true,
}

// RequireCompletion implements pagedStrategy.
func (reviewStrategy) RequireCompletion(item *store.PipelineWorkItem, completionToken string) error {
	if !pagedStepTypes[item.StepType] {
		return nil
	}
	return requireCompletionToken(item.ID, item.FactsJSON, completionToken)
}

// RenderPayload implements pagedStrategy: the facts a paged item ships beside
// its prompt, produced without the prompt.
//
// The round-trip through []factForLLM is not redundant with returning
// item.FactsJSON directly. It is the same construction Render uses — unmarshal
// the stored row, hand it to the Render*WorkItem function, which marshals it —
// so the two paths agree by sharing a derivation rather than by both happening
// to equal the stored bytes. That equality holds today and is what the
// completion token rests on; making it incidental is how it would stop holding.
//
// Everything expensive in Render (methodology retrieval, template execution)
// is absent here by design, which is the entire point of the method.
func (reviewStrategy) RenderPayload(item *store.PipelineWorkItem) (string, error) {
	if !pagedStepTypes[item.StepType] {
		return "", nil
	}
	var facts []factForLLM
	if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
		return "", wrapf(reviewTool, err, "unmarshal facts for page")
	}
	b, err := json.Marshal(facts)
	if err != nil {
		return "", wrapf(reviewTool, err, "marshal facts for page")
	}
	return string(b), nil
}

// distillGroup is one depth-0 distill work unit before chunking: a coherent
// set of seeds plus the key its work items are named after.
type distillGroup struct {
	Key   string
	Facts []factForLLM
}

// distillGroups partitions the seed pool into the groups depth-0 distill
// reasons over, using the clusters Plan already computed.
//
// Two properties of ScopedCluster's output make "just iterate clusters" wrong,
// and this function exists to reconcile them with the seed pool:
//
//   - filterSmallClusters DROPS every community below minCommunitySize, so a
//     seed that clusters alone is absent from clusters entirely. Grouping
//     straight off that output would silently exclude it from synthesis — no
//     error, no counter, indistinguishable from "nothing to synthesize". That
//     is the failure shape this package has already been bitten by twice.
//   - Clusters contain NEIGHBOURS, not just seeds: ScopedCluster pulls the top
//     search hits per seed into the subgraph, and the review path passes no
//     ExcludeTypes, so a cluster can hold facts AcceptSeed deliberately refuses.
//     A pragmatic fact reaching distill would be rewritten as epistemic on
//     commit (decision.go carries no Kind through distillFact) and could be
//     named in `retract`. Restricting to seeds keeps distill's eligible set
//     exactly what the seed scan admitted — widening it is a separate decision
//     with its own hazards, not a side effect of regrouping.
//
// The contract: every seed appears in exactly one returned group. Seeds whose
// community did not survive — and seeds left alone in theirs, where there is
// nothing to synthesize across — collect into a trailing remainder group rather
// than being dropped. A one-fact remainder costs one no-op round trip; a
// dropped seed costs a fact nobody knows is missing.
func distillGroups(seeds []factForLLM, clusters [][]factForLLM) []distillGroup {
	isSeed := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		isSeed[s.File] = true
	}

	claimed := make(map[string]bool, len(seeds))
	var groups []distillGroup
	for _, c := range clusters {
		var g []factForLLM
		for _, f := range c {
			if isSeed[f.File] && !claimed[f.File] {
				claimed[f.File] = true
				g = append(g, f)
			}
		}
		// One seed is not a group — nothing to find a pattern across. Release
		// it so the remainder picks it up.
		if len(g) < 2 {
			for _, f := range g {
				delete(claimed, f.File)
			}
			continue
		}
		groups = append(groups, distillGroup{Key: fmt.Sprintf("distill-c%d", len(groups)), Facts: g})
	}

	// Iterate seeds, not the claimed map: order must not depend on map
	// iteration, or the same corpus would produce different work items run to
	// run and the chunk boundaries would wander with it.
	var remainder []factForLLM
	for _, s := range seeds {
		if !claimed[s.File] {
			remainder = append(remainder, s)
		}
	}
	if len(remainder) > 0 {
		groups = append(groups, distillGroup{Key: "distill-rest", Facts: remainder})
	}
	return groups
}

// factsForLLM projects the engine's canonical seed type onto the prompt-facing
// shape the review path clusters, chunks, and marshals.
//
// The projection is total: factForLLM's fields are a strict subset of
// fact.Fact's. Origin in particular is carried through — bridge seeding
// excludes origin=discovered facts, and dropping it here would let a
// discovered fact seed its own discovery.
func factsForLLM(seeds []fact.Fact) []factForLLM {
	out := make([]factForLLM, 0, len(seeds))
	for _, f := range seeds {
		out = append(out, factForLLM{
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
	return out
}

// OnPhaseAdvance hooks the work→reflect transition to enqueue the session's
// single reflect item. reflect→done needs nothing.
func (reviewStrategy) OnPhaseAdvance(ctx context.Context, d Deps, sess *store.PipelineSession, from, to string) error {
	if from != "work" || to != "reflect" {
		return nil
	}
	return maybeEnqueueReflectItem(ctx, d, sess)
}

// maybeEnqueueReflectItem inserts a single reflect work item iff there are
// hypothesis transitions to reflect on. It is called only from the winner of
// the work→reflect CAS, which is what guarantees at-most-once insertion. A
// failure to detect transitions is logged but not fatal — the session still
// advances (matching the pre-extraction tolerance).
func maybeEnqueueReflectItem(ctx context.Context, d Deps, sess *store.PipelineSession) error {
	transitions, err := findHypothesisTransitions(ctx, d, sess)
	if err != nil {
		log.Warn().Err(err).Str("session", sess.ID).Msg("review: failed to find hypothesis transitions")
		return nil
	}
	if len(transitions) == 0 {
		return nil
	}
	transJSON, err := json.Marshal(transitions)
	if err != nil {
		return wrapf(reviewTool, err, "marshal transitions")
	}
	return d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
		SessionID:  sess.ID,
		StepType:   "reflect",
		ClusterKey: "reflect",
		FactsJSON:  string(transJSON),
		Priority:   reflectPriority,
	})
}

// ── work-item priority bands ──────────────────────────────────────────────

// reflectPriority is the fixed priority of the single "reflect" work item. It
// is the floor of a REVIEW session's negative-priority band: forward "discover"
// items must stay strictly above it so they run before reflect. maxBridgeSeeds
// (bridge.go) caps the discover queue so the rank-derived priority can never
// reach it.
//
// "Floor" is scoped to review deliberately. Hypothesize sessions share this
// package and rank their backward discover items from
// backwardDiscoverPriorityBase — numerically also -100, counting down — so
// package-wide there are items at and below this value. That is not a
// collision: hypothesize never enqueues a reflect item, so the two bands never
// coexist in one session's queue.
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

// ── decode ────────────────────────────────────────────────────────────────

// itemDecision is the decoded, validated product of a response for one review
// work item — everything Apply needs, and nothing that touches the store. The
// engine treats it as opaque; it only shuttles it from the decode half to the
// apply half across the claim CAS.
//
// Exactly one field is populated, selected by the item's step type.
type itemDecision struct {
	reflect  *ReflectResult
	prune    *PruneResult
	distill  *DistillResult
	discover *discoverDecision
}

// Decode parses and validates a response against its work item. It is
// deliberately pure — no store access, no mutation — which is what lets the
// engine run it before claiming the item: any error it returns leaves the item
// fully retryable.
//
// The normalized response is the raw response for every step type that carries
// mandatory content (reflect/prune/distill). Only discover may legitimately be
// answered with nothing, and it substitutes a placeholder — see the case below.
func (reviewStrategy) Decode(item *store.PipelineWorkItem, response string) (any, string, error) {
	switch item.StepType {
	case "reflect":
		var transitions []hypothesisTransition
		if err := json.Unmarshal([]byte(item.FactsJSON), &transitions); err != nil {
			return nil, "", wrapf(reviewTool, err, "unmarshal transitions")
		}
		transitionPaths := make([]string, len(transitions))
		for i, t := range transitions {
			transitionPaths[i] = t.Path
		}
		parsed, err := parseReflectResponse(response)
		if err != nil {
			return nil, "", wrapf(reviewTool, err, "parse reflect response")
		}
		if err := validateReflectResponse(parsed, transitionPaths, reflectProposeCap()); err != nil {
			return nil, "", wrapf(reviewTool, err, "validate reflect")
		}
		return &itemDecision{reflect: &parsed}, response, nil

	case "prune":
		inputPaths, err := itemInputPaths(item)
		if err != nil {
			return nil, "", err
		}
		result, err := parsePruneResponse(response)
		if err != nil {
			return nil, "", wrapf(reviewTool, err, "parse prune response")
		}
		if err := validatePrunePaths(result, inputPaths); err != nil {
			return nil, "", wrapf(reviewTool, err, "validate prune")
		}
		return &itemDecision{prune: &result}, response, nil

	case "distill":
		inputPaths, err := itemInputPaths(item)
		if err != nil {
			return nil, "", err
		}
		result, err := parseDistillResponse(response)
		if err != nil {
			return nil, "", wrapf(reviewTool, err, "parse distill response")
		}
		if err := validateDistillPaths(result, inputPaths); err != nil {
			return nil, "", wrapf(reviewTool, err, "validate distill")
		}
		return &itemDecision{distill: &result}, response, nil

	case "discover":
		// An empty response is "no bridges panned out", not a malformed one.
		// Without this short-circuit the raw "" flows out as the normalized
		// response and trips the engine's empty-response guard, which fires
		// BEFORE the claim — so the item stays unanswered and every retry fails
		// identically at Decode, wedging the session on an item it can never
		// get past. Substituting the acknowledgement placeholder lets the item
		// be claimed and the session advance, which is what the pre-engine
		// review code did (it logged a parse warning and marked the item
		// answered anyway). hypothesizeStrategy.Decode guards the same case.
		if strings.TrimSpace(response) == "" {
			return &itemDecision{discover: &discoverDecision{}}, acknowledgedResponse, nil
		}
		dd, err := decodeDiscoverStep(reviewTool, item, response)
		if err != nil {
			return nil, "", err
		}
		return &itemDecision{discover: dd}, response, nil

	default:
		return nil, "", errf(reviewTool, "unknown step type %q", item.StepType)
	}
}

// itemInputPaths unmarshals a prune/distill item's fact payload and returns
// the paths the response is allowed to reference. Validating against these —
// not against the whole corpus — is what stops a response from acting on
// facts its item never showed the agent.
func itemInputPaths(item *store.PipelineWorkItem) ([]string, error) {
	var facts []factForLLM
	if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
		return nil, wrapf(reviewTool, err, "unmarshal facts")
	}
	paths := make([]string, len(facts))
	for i, f := range facts {
		paths[i] = f.File
	}
	return paths, nil
}

// ── apply ─────────────────────────────────────────────────────────────────

// Apply performs the mutations a decoded response calls for. It runs only
// after the item's claim CAS was won, so it executes at most once per item. An
// error here surfaces to the caller with the item already consumed — see
// Pipeline.ContinueSessionForItem for why that tradeoff is the safe direction.
func (reviewStrategy) Apply(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem, decision any) error {
	dec, ok := decision.(*itemDecision)
	if !ok {
		return errf(reviewTool, "apply: unexpected decision type %T", decision)
	}

	// Use the session's recorded branch — the session was bound to that
	// branch at creation; do not reach into the live AgentBranch.
	branch := sess.Branch

	switch item.StepType {
	case "reflect":
		if err := ApplyReflectDecisions(ctx, d.Facts, d.Search, *dec.reflect, sess, fact.ID12(d.RI.ID()),
			d.RI.OntologyRoot(), reflectNoveltyThreshold(store.EmbedderThresholds(d.RI.Embedder()).ReflectNovelty), d.OnProgress); err != nil {
			return wrapf(reviewTool, err, "apply reflect")
		}

	case "prune":
		stats, err := ApplyPruneDecisions(ctx, d.Facts, d.Search, dec.prune.Decisions, dec.prune.Merges, reviewTool, d.OnProgress, branch, fact.ID12(d.RI.ID()), d.RI.OntologyRoot())
		if err != nil {
			return wrapf(reviewTool, err, "apply prune")
		}
		recordStats(ctx, reviewTool, d, sess, stats)

	case "distill":
		stats, writtenFacts, err := ApplyDistillDecisions(ctx, d.Facts, d.Search, dec.distill.Synthesize, dec.distill.Retract, reviewTool, d.OnProgress, branch, fact.ID12(d.RI.ID()), d.RI.OntologyRoot())
		if err != nil {
			return wrapf(reviewTool, err, "apply distill")
		}
		recordStats(ctx, reviewTool, d, sess, stats)
		enqueueRaptorFollowups(ctx, d, sess, item, writtenFacts)

	case "discover":
		return applyDiscoverStep(ctx, reviewTool, d, sess, dec.discover)

	default:
		return errf(reviewTool, "unknown step type %q", item.StepType)
	}
	return nil
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
func enqueueRaptorFollowups(
	ctx context.Context,
	d Deps,
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
	raptorClusters, clErr := ScopedCluster(ctx, newFacts, d.Search, d.RI.ClusterResolution(), d.RI.ClusterMinCommunitySize(), d.OnProgress, sess.Branch, "hypothesis")
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
		for chi, chunk := range chunkFacts(cluster, maxItemBytes) {
			factsJSON, _ := json.Marshal(chunk)
			wItem := store.PipelineWorkItem{
				SessionID:  sess.ID,
				StepType:   "distill",
				ClusterKey: fmt.Sprintf("raptor-d%d-c%d-%d", nextDepth, ci, chi),
				FactsJSON:  string(factsJSON),
				Priority:   float64(-nextDepth),
				Depth:      nextDepth,
			}
			if err := d.Pipeline.InsertPipelineWorkItem(ctx, wItem); err != nil {
				log.Warn().Err(err).Msg("review: RAPTOR enqueue failed")
			}
		}
	}
	if len(raptorClusters) > 0 {
		log.Info().Int("depth", nextDepth).Int("clusters", len(raptorClusters)).Msg("review: RAPTOR enqueued deeper distill items")
	}
}

// ── render ────────────────────────────────────────────────────────────────

// Render builds the prompt and response schema for one review work item.
// Pure presentation; the engine decides whether to call this or advance the
// phase, and attaches the item id and progress counts afterwards.
func (reviewStrategy) Render(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem) (*WorkItemView, error) {
	branch := sess.Branch
	ontologyRoot := d.RI.OntologyRoot()

	var content *WorkItemContent
	var err error
	switch item.StepType {
	case "prune":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, wrapf(reviewTool, err, "unmarshal facts for prompt")
		}
		content, err = RenderPruneWorkItem(facts, ontologyRoot)
	case "distill":
		var facts []factForLLM
		if err := json.Unmarshal([]byte(item.FactsJSON), &facts); err != nil {
			return nil, wrapf(reviewTool, err, "unmarshal facts for prompt")
		}
		applicableMethodology := distillMethodologySection(ctx, d.RI, branch, facts)
		content, err = RenderDistillWorkItem(facts, ontologyRoot, applicableMethodology)
	case "reflect":
		existingMethodology := reflectMethodologySection(ctx, d.RI, branch, []byte(item.FactsJSON))
		content, err = RenderReflectWorkItem([]byte(item.FactsJSON), ontologyRoot, existingMethodology)
	case "discover":
		var payload DiscoverWorkPayload
		if uerr := json.Unmarshal([]byte(item.FactsJSON), &payload); uerr != nil {
			return nil, wrapf(reviewTool, uerr, "unmarshal discover payload for prompt")
		}
		content = RenderDiscoverWorkItem(payload, ontologyRoot)
	default:
		return nil, errf(reviewTool, "unknown step type %q", item.StepType)
	}
	if err != nil {
		return nil, wrapf(reviewTool, err, "render %s prompt", item.StepType)
	}

	return &WorkItemView{
		Type:           item.StepType,
		Prompt:         content.Prompt,
		ResponseSchema: content.ResponseSchema,
		Facts:          content.Facts,
	}, nil
}

// ── methodology retrieval ─────────────────────────────────────────────────

// methodologyTopK is the per-fact retrieval depth and the final merged-list
// cap; both share one knob since callers want "show up to N methodologies"
// in the prompt regardless of cluster size.
const methodologyTopK = 3

// reflectMethodologySection retrieves methodology relevant to each transitioned
// hypothesis fact independently, then merges the per-fact results (keeping
// the highest score per methodology path). Returns "" on any failure (logged)
// or when no methodology matches.
//
// branch is required (no implicit AgentBranch fallback): callers pass the
// session's recorded branch so retrieval lands on the same branch the
// session was created against.
func reflectMethodologySection(ctx context.Context, ri *repos.RepoInstance, branch string, transitionsJSON []byte) string {
	var ts []hypothesisTransition
	if err := json.Unmarshal(transitionsJSON, &ts); err != nil {
		log.Warn().Err(err).Msg("loadReflectMethodology: transitions JSON malformed; skipping methodology section")
		return ""
	}
	if len(ts) == 0 {
		return ""
	}

	var merged []store.MethodologyMatch
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			log.Error().Str("branch", branch).Msg("loadReflectMethodology: nil store service; methodology disabled")
			return
		}
		minScore := ri.MethodologyMinScore()
		seen := map[string]store.MethodologyMatch{}
		for _, t := range ts {
			if err := ctx.Err(); err != nil {
				log.Warn().Err(err).Str("branch", branch).Str("path", t.Path).
					Msg("loadReflectMethodology: ctx canceled mid-iteration")
				return
			}
			f, err := svc.FactQuery().GetByPath(ctx, branch, t.Path)
			if err != nil {
				log.Warn().Err(err).Str("branch", branch).Str("path", t.Path).
					Msg("loadReflectMethodology: transition fact lookup failed; skipping")
				continue
			}
			if f == nil {
				continue
			}
			matches, mErr := svc.Methodology().RelevantMethodologyForFact(
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

// distillMethodologySection retrieves methodology relevant to each input fact
// independently, then merges the per-fact results. Returns "" on failure
// (logged) or when no methodology matches.
//
// branch is required (no implicit AgentBranch fallback).
func distillMethodologySection(ctx context.Context, ri *repos.RepoInstance, branch string, facts []factForLLM) string {
	if len(facts) == 0 {
		return ""
	}

	var merged []store.MethodologyMatch
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			log.Error().Str("branch", branch).Msg("loadDistillMethodology: nil store service; methodology disabled")
			return
		}
		minScore := ri.MethodologyMinScore()
		seen := map[string]store.MethodologyMatch{}
		for _, f := range facts {
			if err := ctx.Err(); err != nil {
				log.Warn().Err(err).Str("branch", branch).Str("path", f.File).
					Msg("loadDistillMethodology: ctx canceled mid-iteration")
				return
			}
			matches, mErr := svc.Methodology().RelevantMethodologyForFact(
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

// ── hypothesis transitions ────────────────────────────────────────────────

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
func findHypothesisTransitions(ctx context.Context, d Deps, sess *store.PipelineSession) ([]hypothesisTransition, error) {
	gs := d.Facts
	branch := sess.Branch

	// Read the watermark set at the end of the previous session.
	// Since we haven't advanced it yet (that happens in completeSession),
	// all commits between here and HEAD are changes made during this session.
	watermark, err := d.Pipeline.GetPipelineWatermark(ctx, reviewTool, branch)
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
