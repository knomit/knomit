// Package synthesize — hypothesizeStrategy: the hypothesize tool's half of the
// engine.
//
// A hypothesize session walks the corpus's synthesis facts one at a time and
// asks the agent whether each one warrants a forward-looking, falsifiable
// hypothesis. At effort >= medium it additionally enqueues BACKWARD discovery
// items: bridges over the synthesis-fact pool that invite the agent to propose
// the unstated KEYSTONE hypothesis that would entail the bridged facts.
//
// Everything else — the session lifecycle, the seed scan and its watermark
// gate, the phase machine, the claim protocol, completion and the scoped
// watermark suppression — is the engine's (pipeline.go). Before the extraction
// this file's contents lived in internal/mcp/hypothesize.go as a second,
// hand-maintained copy of that machinery; the copy is what let hypothesize
// drift into reading the live agent branch mid-session
// (invariants/synthesize/session-branch-binding).
//
// hypothesizeStrategy is a zero-size struct on purpose, exactly like
// reviewStrategy: it is rebuilt per MCP call alongside its Pipeline, so a field
// here would be invisible across turns
// (invariants/synthesize/per-call-objects-no-session-state).
package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"

	"github.com/rs/zerolog/log"
)

// hypothesizeStrategy implements Strategy for the hypothesis-generation
// pipeline: one acknowledgement item per synthesis fact, plus optional
// backward-discovery items.
type hypothesizeStrategy struct{}

// hypothesizeTool is the pipeline_sessions.tool value and the watermark key.
// It is namespaced separately from reviewTool, which is what keeps the two
// tools' incremental state from corrupting each other.
const hypothesizeTool = "hypothesize"

// NewHypothesizer builds the hypothesize pipeline: effort + optional scope
// filter, mirroring NewReviewerWithOptions.
//
// Unlike review, hypothesize has no facade type wrapping the engine — its MCP
// layer projects PipelineResult straight onto the HypothesizeResult wire shape,
// so there is nothing for a facade to add. Callers drive *Pipeline directly.
func NewHypothesizer(ri *repos.RepoInstance, onProgress func(ProgressEvent), effort Effort, scope ScopeFilter) *Pipeline {
	return NewPipeline(ri, onProgress, effort, scope, hypothesizeStrategy{})
}

func (hypothesizeStrategy) Tool() string { return hypothesizeTool }

// SeedQuery scans the whole corpus for synthesis facts.
//
// The type restriction is pushed into SQL purely as an efficiency measure;
// AcceptSeed repeats it and is the authoritative statement of the rule.
func (hypothesizeStrategy) SeedQuery() store.SearchOptions {
	return store.SearchOptions{
		Limit:        100_000,
		IncludeTypes: []string{string(fact.Synthesis)},
	}
}

// AcceptSeed keeps everything that is not a synthesis fact out of the pool:
// hypothesize extends *already-synthesized* knowledge forward, so an
// observation or an existing hypothesis is not a seed.
//
// This predicate — not SeedQuery — is authoritative, because SQL cannot run on
// the incremental (DiffFiles) scan path. Both paths must apply the same rule,
// or the seed pool would depend on whether a watermark happens to exist.
func (hypothesizeStrategy) AcceptSeed(f fact.Fact) bool {
	return f.Type == fact.Synthesis
}

// Plan enqueues one acknowledgement work item per synthesis fact, then — at
// effort >= medium with at least two seeds — the backward-bridge discover
// items.
//
// Priority counts DOWN from len(seeds), so the pool is served in scan order
// (NextPipelineWorkItem orders priority DESC) and every per-fact item keeps a
// strictly positive priority. That positivity is load-bearing: the discover
// band below is defined as "strictly negative", which is only a meaningful
// separation while the per-fact band stays above zero.
//
// branch comes off sess, never off d.RI: the session is already bound to the
// branch it was created against.
func (hypothesizeStrategy) Plan(ctx context.Context, d Deps, sess *store.PipelineSession, seeds []fact.Fact) error {
	branch := sess.Branch

	for i, f := range seeds {
		factJSON, err := json.Marshal(f)
		if err != nil {
			return wrapf(hypothesizeTool, err, "marshal synthesis fact %d", i)
		}
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "hypothesize",
			ClusterKey: fmt.Sprintf("synth-%d", i),
			FactsJSON:  string(factJSON),
			Priority:   float64(len(seeds) - i),
		}
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, item); err != nil {
			return wrapf(hypothesizeTool, err, "insert work item")
		}
	}

	// Backward discovery: at effort >= medium, build bridge seeds from the
	// synthesis-fact pool and enqueue a 'discover' item per bridge asking the
	// agent to propose the unstated KEYSTONE hypothesis that would entail the
	// bridged facts. A single seed cannot bridge to anything, hence the >= 2
	// guard. Skipped entirely at EffortNormal — that is the zero-discovery-spend
	// contract of invariants/synthesize/effort-normal-byte-identical.
	switch {
	case d.Effort.Discovers() && len(seeds) >= 2:
		if err := enqueueBackwardBridgeItems(ctx, d, sess.ID, seeds, branch); err != nil {
			// Non-fatal: discovery is enrichment, not a blocker on the standard
			// per-fact flow, and the grounded work above is already queued.
			log.Warn().Err(err).Str("session", sess.ID).
				Msg("hypothesize: backward bridge enqueue failed; continuing without discovery items")
		}
	case d.Effort.Discovers():
		log.Debug().Str("session", sess.ID).Int("synth_facts", len(seeds)).
			Msg("hypothesize: backward discovery skipped; need ≥2 synthesis facts in scope")
	}

	log.Info().Str("session", sess.ID).Int("seeds", len(seeds)).
		Str("effort", string(d.Effort)).Msg("hypothesize: work planned")
	d.OnProgress(ProgressEvent{
		Phase:   "hypothesize-start",
		Message: fmt.Sprintf("session %s: %d synthesis facts", sess.ID, len(seeds)),
	})
	return nil
}

// OnPhaseAdvance is a no-op: hypothesize has no reflect step.
//
// The engine still walks work→reflect→done for a hypothesize session, which
// costs two CAS updates and one extra empty queue probe on the completing turn
// and is otherwise inert — nothing ever enqueues an item during the reflect
// phase, so handlePhase("reflect","done") finds an empty queue and falls
// straight through to completion.
func (hypothesizeStrategy) OnPhaseAdvance(context.Context, Deps, *store.PipelineSession, string, string) error {
	return nil
}

// ── work-item priority bands ──────────────────────────────────────────────

// backwardDiscoverPriorityBase places backward "discover" work items below the
// per-fact hypothesize band, whose items carry positive priorities
// (NextPipelineWorkItem orders priority DESC). Discovery is low-priority
// enrichment that must run only after the grounded per-fact work.
const backwardDiscoverPriorityBase = -100

// backwardDiscoverPriority ranks the i-th backward "discover" item. The caller
// sorts bridges by BlastRadius descending, so rank == i preserves that
// keystone order WITHIN the discover band while keeping every priority strictly
// negative.
//
// Crucially, priority is a function of RANK, not of the BlastRadius magnitude:
// feeding blast straight into the priority (the old `-100 + blast` anti-pattern)
// let a high-blast keystone produce a positive priority and leapfrog the
// per-fact items it must run after. Mirrors the forward path's
// forwardDiscoverPriority, which was written to avoid the same flip.
func backwardDiscoverPriority(rank int) float64 {
	return backwardDiscoverPriorityBase - float64(rank)
}

// enqueueBackwardBridgeItems clusters the synthesis-fact pool in-process
// (BuildBackwardBridges → ScopedCluster), then enqueues one 'discover' work
// item per bridge, ranked by BlastRadius descending (high blast = high backward
// priority).
//
// seeds is already scope-filtered by the engine's seed scan; the bridge engine
// caps the result by effort budget (medium=12, high=48) regardless. bridgeKind,
// resolution and minCommunitySize all come from the same per-repo config the
// forward (review) path uses, so both discovery directions honour the same axis
// selection AND the same community partition, with nothing hardcoded.
func enqueueBackwardBridgeItems(ctx context.Context, d Deps, sessionID string, seeds []fact.Fact, branch string) error {
	bridges, err := BuildBackwardBridges(ctx, d.Search, seeds, branch, d.Effort,
		BridgeKindFromString(d.RI.DiscoveryBridge()), d.RI.ClusterResolution(),
		d.RI.ClusterMinCommunitySize(), QualityConfigFromRepo(d.RI), d.Scope)
	if err != nil {
		return err
	}

	// Order discover-bwd items by BlastRadius descending: higher impact
	// keystones are seen first. The same member path commonly appears across
	// several bridges, and BlastRadius runs a recursive CTE plus a liveness
	// query per transitive dependent, so memoize per path to avoid recomputing
	// the same walk on every bridge it participates in.
	type rankedBridge struct {
		b    BridgeSeedSet
		rank int
	}
	blastByPath := make(map[string]int)
	blastOf := func(path string) int {
		if br, ok := blastByPath[path]; ok {
			return br
		}
		br, err := d.Search.BlastRadius(ctx, branch, path)
		if err != nil {
			br = 0
		}
		blastByPath[path] = br
		return br
	}
	ranked := make([]rankedBridge, 0, len(bridges))
	for _, b := range bridges {
		maxBlast := 0
		for _, m := range b.Members {
			if br := blastOf(m.File); br > maxBlast {
				maxBlast = br
			}
		}
		ranked = append(ranked, rankedBridge{b: b, rank: maxBlast})
	}
	// Stable sort by descending rank; ties by token name.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].rank != ranked[j].rank {
			return ranked[i].rank > ranked[j].rank
		}
		return ranked[i].b.Token < ranked[j].b.Token
	})

	sl := scopeLabel(d.Scope)
	for i, rb := range ranked {
		payload := DiscoverWorkPayload{Direction: DiscoverBackward, Bridge: rb.b, ScopeLabel: sl}
		payloadJSON, mErr := json.Marshal(payload)
		if mErr != nil {
			return wrapf(hypothesizeTool, mErr, "marshal backward discover %d", i)
		}
		// Discover items must run AFTER the whole per-fact hypothesize loop,
		// whose items carry positive priorities (NextPipelineWorkItem orders
		// priority DESC). `ranked` is already sorted by BlastRadius descending,
		// so backwardDiscoverPriority(i) keeps the high-blast keystones first
		// WITHIN the discover band while guaranteeing every discover item stays
		// strictly negative — a large BlastRadius can no longer flip the priority
		// positive and leapfrog the standard items (the old -100+rank bug).
		if err := d.Pipeline.InsertPipelineWorkItem(ctx, store.PipelineWorkItem{
			SessionID:  sessionID,
			StepType:   "discover",
			ClusterKey: fmt.Sprintf("discover-bwd-%d", i),
			FactsJSON:  string(payloadJSON),
			Priority:   backwardDiscoverPriority(i),
		}); err != nil {
			return wrapf(hypothesizeTool, err, "insert backward discover %d", i)
		}
	}
	return nil
}

// ── decode / apply ────────────────────────────────────────────────────────

// acknowledgedResponse is the placeholder stored for a work item the agent
// answered with nothing. The claim CAS keys on `response IS NULL`, so an item
// MUST end up with non-empty text or "was this answered" becomes ambiguous —
// see Strategy.Decode's normalized return.
const acknowledgedResponse = "acknowledged"

// hypothesizeDecision is the decoded form of a response for one hypothesize
// work item. discover is nil for the per-fact "hypothesize" step, and also for
// a "discover" step the agent declined to answer.
type hypothesizeDecision struct {
	discover *discoverDecision
}

// Decode parses and validates a response against its work item. Pure, as the
// engine requires: it runs before the claim CAS, so any error it returns leaves
// the item retryable.
//
// The per-fact "hypothesize" step carries no decodable content at all — the
// agent does its work through other tools (knomit_learn) and the item is a pure
// acknowledgement that it has moved on. An empty answer is therefore legitimate
// and normalizes to the acknowledgement placeholder rather than being rejected.
func (hypothesizeStrategy) Decode(item *store.PipelineWorkItem, response string) (any, string, error) {
	normalized := response
	if normalized == "" {
		normalized = acknowledgedResponse
	}

	switch item.StepType {
	case "hypothesize":
		return &hypothesizeDecision{}, normalized, nil

	case "discover":
		// An empty response is "no proposals", not a malformed one: short-circuit
		// so it is recorded as an acknowledgement instead of being run through
		// the parser purely to log a warning about text that was never there.
		if strings.TrimSpace(response) == "" {
			return &hypothesizeDecision{}, normalized, nil
		}
		dd, err := decodeDiscoverStep(hypothesizeTool, item, response)
		if err != nil {
			return nil, "", err
		}
		return &hypothesizeDecision{discover: dd}, normalized, nil

	default:
		return nil, "", errf(hypothesizeTool, "unknown step type %q", item.StepType)
	}
}

// Apply mutates the corpus for a decoded response. It runs only after the claim
// CAS was won, so it executes at most once per item.
//
// The per-fact "hypothesize" step mutates nothing: any hypothesis the agent
// decided to write it wrote itself through knomit_learn, outside this session.
// Only the discover step has anything to apply.
func (hypothesizeStrategy) Apply(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem, decision any) error {
	dec, ok := decision.(*hypothesizeDecision)
	if !ok {
		return errf(hypothesizeTool, "apply: unexpected decision type %T", decision)
	}
	if dec.discover == nil {
		return nil
	}
	return applyDiscoverStep(ctx, hypothesizeTool, d, sess, dec.discover)
}

// ── render ────────────────────────────────────────────────────────────────

// Render builds the agent-facing view of one hypothesize work item. The engine
// attaches the item id, its raw payload (which the MCP facade surfaces as the
// `fact` field) and the progress counts afterwards.
//
// Neither step type carries a response schema: a hypothesize item is answered
// with a bare acknowledgement, and the discover prompt embeds its own output
// contract.
func (hypothesizeStrategy) Render(ctx context.Context, d Deps, sess *store.PipelineSession, item *store.PipelineWorkItem) (*WorkItemView, error) {
	branch := sess.Branch

	switch item.StepType {
	case "hypothesize":
		// A malformed payload degrades to an empty methodology section rather
		// than failing the turn: the item's raw JSON still reaches the agent as
		// the `fact` field, so the work is doable without the section.
		var synthFact fact.Fact
		if err := json.Unmarshal([]byte(item.FactsJSON), &synthFact); err != nil {
			log.Warn().Err(err).Str("session", sess.ID).
				Msg("hypothesize: unmarshal synth fact failed; methodology section will be empty")
		}
		return &WorkItemView{
			Type:   "hypothesize",
			Prompt: buildHypothesizeInstructions(ctx, d.RI, branch, synthFact.Path()),
		}, nil

	case "discover":
		var payload DiscoverWorkPayload
		if err := json.Unmarshal([]byte(item.FactsJSON), &payload); err != nil {
			return nil, wrapf(hypothesizeTool, err, "unmarshal discover payload for prompt")
		}
		return &WorkItemView{
			Type:   "discover",
			Prompt: RenderDiscoverWorkItem(payload, d.RI.OntologyRoot()).Prompt,
		}, nil

	default:
		return nil, errf(hypothesizeTool, "unknown step type %q", item.StepType)
	}
}

// buildHypothesizeInstructions returns the step-by-step instructions for the
// agent. When relevant methodology exists on the branch, it is rendered as
// the FIRST thing the LLM sees so it lands in working context as input,
// not as an appendix the model can skim past after committing to a plan.
//
// branch is required and must be the same branch the synthesis fact lives
// on; it is not derived from ri.AgentBranch() so the caller cannot
// silently retrieve from the wrong branch.
func buildHypothesizeInstructions(ctx context.Context, ri *repos.RepoInstance, branch, synthPath string) string {
	section := hypothesizeMethodologySection(ctx, ri, branch, synthPath)

	if section == "" {
		// No methodology on branch — simpler workflow with no fetch step.
		return `WORKFLOW (do not skip steps):

1. Call knomit_explain on the synthesis fact to trace its provenance.
2. Gather evidence as needed via knomit_query.
3. Decide whether a hypothesis is warranted. Default to NO. Write one ONLY if ALL of these hold:
   (a) Forward-looking: the hypothesis predicts or causally claims something beyond what the synth fact already establishes. Restating the synth fact is not a hypothesis.
   (b) Falsifiable: you can state a concrete settlement criterion (date, threshold, or observable). "Trends will continue" does not qualify.
   (c) Load-bearing gap: there is a specific piece of evidence whose discovery would meaningfully shift the hypothesis's confidence.
   (d) Not duplicative: no existing hypothesis on this branch already makes substantively the same prediction. If unsure, briefly check via knomit_query.
   If any condition fails, skip — proceed directly to step 6. Skipping is the expected outcome for most synth facts.
4. If you decided yes in step 3: call knomit_learn with type: hypothesis. The refs array MUST include the synthesis fact's path AND every source fact you cite as evidence. An empty refs array indicates you did not engage with the inputs — do not submit.
5. If you wrote a hypothesis in step 4: call knomit_learn with type: methodology, topic: "meta", category: "reasoning" to record the reasoning process you used. Set the methodology's domain and entities to the union of the source synthesis fact's tags plus the standard markers (meta, reasoning, methodology).
6. Call knomit_hypothesize with session_id to continue to the next synthesis fact.`
	}

	return section + `

WORKFLOW (do not skip steps):

1. Call knomit_explain on the synthesis fact to trace its provenance.
2. For EVERY methodology candidate above with score ≥ 0.50, call knomit_query on its path and read the body. Decide whether it applies to your reasoning here. Titles alone are not enough to judge applicability — do not skip candidates above the threshold.
3. Gather additional evidence as needed via knomit_query.
4. Decide whether a hypothesis is warranted. Default to NO. Write one ONLY if ALL of these hold:
   (a) Forward-looking: the hypothesis predicts or causally claims something beyond what the synth fact already establishes. Restating the synth fact is not a hypothesis.
   (b) Falsifiable: you can state a concrete settlement criterion (date, threshold, or observable). "Trends will continue" does not qualify.
   (c) Load-bearing gap: there is a specific piece of evidence whose discovery would meaningfully shift the hypothesis's confidence.
   (d) Not duplicative: no existing hypothesis on this branch already makes substantively the same prediction. If unsure, briefly check via knomit_query.
   If any condition fails, skip — proceed directly to step 7. Skipping is the expected outcome for most synth facts.
5. If you decided yes in step 4: call knomit_learn with type: hypothesis. The refs array MUST include:
   - the synthesis fact's path
   - every source fact you cite as evidence
   - every methodology from step 2 that shaped your reasoning
   An empty refs array indicates you did not engage with the inputs — do not submit.
6. If you wrote a hypothesis in step 5: only call knomit_learn with type: methodology if your reasoning is GENUINELY novel. If a methodology you read in step 2 already captures the same lesson, skip the new methodology fact — adding a near-duplicate pollutes the methodology pool and dilutes future retrieval. When you do write one, set domain and entities to the union of the source synthesis fact's tags plus the standard markers (meta, reasoning, methodology).
7. Call knomit_hypothesize with session_id to continue to the next synthesis fact.`
}

// hypothesizeMethodologySection queries the branch's methodology for the given
// synthesis fact and renders it as a prompt-ready section. Returns "" when
// no methodology is relevant or any lookup fails — failures are logged.
//
// This is the single-fact sibling of distillMethodologySection and
// reflectMethodologySection (review_strategy.go). It is not folded into either:
// both of those merge per-fact results over a whole item payload and take the
// domain/entities straight off the payload, whereas an item here names ONE fact
// and resolves its canonical domain/entities via GetByPath first — the payload
// was serialized when the session started and may already be stale.
//
// branch is required (no implicit AgentBranch fallback): callers must
// pass the branch the synth fact was fetched from so retrieval lands on
// the same branch.
func hypothesizeMethodologySection(ctx context.Context, ri *repos.RepoInstance, branch, synthPath string) string {
	var matches []store.MethodologyMatch
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			log.Error().Str("branch", branch).Str("synth_path", synthPath).
				Msg("hypothesize: nil store service; methodology disabled")
			return
		}
		f, err := svc.FactQuery().GetByPath(ctx, branch, synthPath)
		if err != nil {
			log.Warn().Err(err).Str("branch", branch).Str("synth_path", synthPath).
				Msg("hypothesize: synth fact lookup failed; methodology section skipped")
			return
		}
		if f == nil {
			return
		}
		var mErr error
		matches, mErr = svc.Methodology().RelevantMethodologyForFact(
			ctx, branch,
			f.Path, f.Domain, f.Entities,
			methodologyTopK, ri.MethodologyMinScore(),
		)
		if mErr != nil {
			log.Warn().Err(mErr).Str("branch", branch).Str("synth_path", synthPath).
				Msg("hypothesize: methodology retrieval failed; continuing without section")
		}
	})
	bullets := store.FormatMethodologySection(matches)
	if bullets == "" {
		return ""
	}
	return "Applicable methodology candidates (ranked; you must process the ≥0.50 ones per workflow step 2):\n\n" + bullets
}

// compile-time assertion that the hypothesize strategy satisfies the engine seam.
var _ Strategy = hypothesizeStrategy{}
