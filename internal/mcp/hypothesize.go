package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// HypothesizeResult is the JSON response returned by the hypothesize tool.
type HypothesizeResult struct {
	SessionID string               `json:"session_id"`
	Item      *HypothesizeItem     `json:"item,omitempty"`
	Done      bool                 `json:"done"`
	Progress  *HypothesizeProgress `json:"progress,omitempty"`
}

// HypothesizeItem describes a single synthesis fact to evaluate for hypothesis generation.
type HypothesizeItem struct {
	Type         string          `json:"type"`
	Fact         json.RawMessage `json:"fact"`
	Instructions string          `json:"instructions"`
}

// HypothesizeProgress tracks completed/remaining counts.
type HypothesizeProgress struct {
	Completed int `json:"completed"`
	Remaining int `json:"remaining"`
}

// hypothesizeTool returns the Tool definition for knomit_hypothesize.
func hypothesizeTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_hypothesize",
		mcpgo.WithDescription("Generate NEW hypothesis facts from synthesis facts on the agent branch. This is a distinct operation from knomit_review — only invoke when the user has explicitly asked to hypothesize, generate predictions, or extend synthesis facts forward. Do NOT invoke as a follow-up to knomit_review or other maintenance tools without an explicit user request. Each work item presents one synthesis fact; the agent decides per-item whether to write a hypothesis (skipping is the expected outcome for most synth facts — see workflow). Call with no arguments to start a new session. Call with session_id to continue processing the next fact."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your response/acknowledgement for the previous work item.")),
		mcpgo.WithString("effort", mcpgo.Description("Discovery effort dial: 'normal' (default), 'medium', or 'high'. Medium/high engage the structural-bridge engine for emergent keystone-hypothesis discovery (backward direction).")),
		mcpgo.WithArray("domain", mcpgo.Description("Optional scope filter: restrict the synthesis-fact seed pool to these domains. Empty = whole corpus.")),
		mcpgo.WithArray("entities", mcpgo.Description("Optional scope filter: restrict the synthesis-fact seed pool to facts tagged with these entities. Empty = whole corpus.")),
	)
}

// HypothesizeHandler returns the handler function for knomit_hypothesize.
func HypothesizeHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		ri := repos.RepoFromContext(ctx)
		s := storeIndices(ri)
		agentBranch := ri.AgentBranch()
		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		effort := synthesize.Effort(req.GetString("effort", ""))
		if effort != "" {
			if verr := effort.Validate(); verr != nil {
				return mcpgo.NewToolResultError(verr.Error()), nil
			}
		}
		effort = synthesize.NormalizeEffort(effort)

		scope := synthesize.ScopeFilter{
			Domain:   req.GetStringSlice("domain", nil),
			Entities: req.GetStringSlice("entities", nil),
		}

		var result *HypothesizeResult
		var err error

		if sessionID == "" {
			result, err = hypothesizeStart(ctx, ri, s, agentBranch, effort, scope)
		} else {
			result, err = hypothesizeContinue(ctx, ri, s, agentBranch, sessionID, response)
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("hypothesize error: %v", err)), nil
		}

		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}

// hypothesizeStart creates a new session, finds synthesis facts, and returns the first item.
// effort controls whether the discovery engine engages (medium/high) or the
// pre-discovery flow runs byte-for-byte (normal). scope optionally restricts
// the seed pool to facts touching the listed domains/entities.
func hypothesizeStart(ctx context.Context, ri *repos.RepoInstance, s mcpStore, agentBranch string, effort synthesize.Effort, scope synthesize.ScopeFilter) (*HypothesizeResult, error) {
	branch := agentBranch
	_ = effort // future: discovery engine will branch on effort.Discovers()

	// Get watermark.
	watermark, err := s.pipeline.GetPipelineWatermark(ctx, "hypothesize", branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	var synthFacts []fact.Fact

	if watermark == "" {
		// First run: search for all synthesis facts (optionally scoped to a
		// domain/entity pool — empty filter = whole-corpus).
		results, err := s.search.Search(ctx, agentBranch, store.SearchOptions{
			IncludeTypes: []string{"synthesis"},
			Limit:        100000,
			Domain:       scope.Domain,
			Entities:     scope.Entities,
		})
		if err != nil {
			return nil, fmt.Errorf("search synthesis facts: %w", err)
		}
		for _, r := range results {
			sf := fact.NewFact(r.Path)
			sf.Title = r.Title
			sf.Body = r.Body
			sf.Type = fact.Type(r.Type)
			sf.Domain = r.Domain
			sf.Confidence = r.Confidence
			sf.Sources = r.Sources
			sf.Entities = r.Entities
			synthFacts = append(synthFacts, sf)
		}
	} else {
		// Incremental: find changed files since watermark.
		added, modified, _, err := s.facts.DiffFiles(ctx, agentBranch, watermark)
		if err != nil {
			return nil, fmt.Errorf("diff files: %w", err)
		}
		changedPaths := append(added, modified...)
		for _, p := range changedPaths {
			if !strings.HasSuffix(p, ".md") {
				continue
			}
			readResult, readErr := s.facts.ReadFact(ctx, agentBranch, p, nil)
			if readErr != nil {
				continue
			}
			f, parseErr := fact.ParseFact(p, readResult.Content)
			if parseErr != nil {
				continue
			}
			if string(f.Type) == "synthesis" {
				synthFacts = append(synthFacts, f)
			}
		}
	}

	// No synthesis facts → done immediately.
	if len(synthFacts) == 0 {
		// Advance watermark even when empty so next run is incremental.
		if head, err := s.branches.HeadCommit(ctx, agentBranch); err == nil {
			_ = s.pipeline.SetPipelineWatermark(ctx, "hypothesize", branch, head)
		}
		return &HypothesizeResult{Done: true}, nil
	}

	// Create session, recording the effort dial so a later ContinueSession
	// can recover it.
	sess, err := s.pipeline.CreatePipelineSessionWithEffort(ctx, "hypothesize", branch, string(effort))
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	// Create one work item per synthesis fact.
	for i, f := range synthFacts {
		factJSON, _ := json.Marshal(f)
		item := store.PipelineWorkItem{
			SessionID:  sess.ID,
			StepType:   "hypothesize",
			ClusterKey: fmt.Sprintf("synth-%d", i),
			FactsJSON:  string(factJSON),
			Priority:   float64(len(synthFacts) - i),
		}
		if err := s.pipeline.InsertPipelineWorkItem(ctx, item); err != nil {
			return nil, fmt.Errorf("insert work item: %w", err)
		}
	}

	return hypothesizeNextItem(ctx, ri, s, agentBranch, sess.ID)
}

// hypothesizeContinue acknowledges the current work item and advances to the next.
func hypothesizeContinue(ctx context.Context, ri *repos.RepoInstance, s mcpStore, agentBranch, sessionID, response string) (*HypothesizeResult, error) {
	// Verify session exists and is active.
	sess, err := s.pipeline.GetPipelineSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	if sess.Status != "active" {
		return nil, fmt.Errorf("session %q is %s, not active", sessionID, sess.Status)
	}

	// Get current unanswered work item and mark it as answered.
	current, err := s.pipeline.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get current item: %w", err)
	}
	if current != nil {
		resp := response
		if resp == "" {
			resp = "acknowledged"
		}
		if err := s.pipeline.SetPipelineWorkItemResponse(ctx, current.ID, resp); err != nil {
			return nil, fmt.Errorf("set response: %w", err)
		}
	}

	return hypothesizeNextItem(ctx, ri, s, agentBranch, sessionID)
}

// hypothesizeNextItem fetches the next unanswered work item or completes the session.
func hypothesizeNextItem(ctx context.Context, ri *repos.RepoInstance, s mcpStore, agentBranch, sessionID string) (*HypothesizeResult, error) {
	item, err := s.pipeline.NextPipelineWorkItem(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("next item: %w", err)
	}

	// No more items → complete session and advance watermark.
	if item == nil {
		if err := s.pipeline.CompletePipelineSession(ctx, sessionID); err != nil {
			return nil, fmt.Errorf("complete session: %w", err)
		}
		if head, err := s.branches.HeadCommit(ctx, agentBranch); err == nil {
			_ = s.pipeline.SetPipelineWatermark(ctx, "hypothesize", agentBranch, head)
		}
		return &HypothesizeResult{
			SessionID: sessionID,
			Done:      true,
		}, nil
	}

	// Extract the synthesis fact's path from the work-item JSON so we can
	// query relevant methodology for it.
	var synthFact fact.Fact
	if err := json.Unmarshal([]byte(item.FactsJSON), &synthFact); err != nil {
		log.Warn().Err(err).Msg("hypothesize: unmarshal synth fact failed; methodology section will be empty")
	}
	instructions := buildHypothesizeInstructions(ctx, ri, agentBranch, synthFact.Path())

	completed, remaining, _ := s.pipeline.PipelineWorkItemStats(ctx, sessionID)

	return &HypothesizeResult{
		SessionID: sessionID,
		Item: &HypothesizeItem{
			Type:         "hypothesize",
			Fact:         json.RawMessage(item.FactsJSON),
			Instructions: instructions,
		},
		Done: false,
		Progress: &HypothesizeProgress{
			Completed: completed,
			Remaining: remaining,
		},
	}, nil
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
	section := loadMethodologySection(ctx, ri, branch, synthPath)

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

// loadMethodologySection queries the branch's methodology for the given
// synthesis fact and renders it as a prompt-ready section. Returns "" when
// no methodology is relevant or any lookup fails — failures are logged.
//
// branch is required (no implicit AgentBranch fallback): callers must
// pass the branch the synth fact was fetched from so retrieval lands on
// the same branch.
func loadMethodologySection(ctx context.Context, ri *repos.RepoInstance, branch, synthPath string) string {
	var matches []store.MethodologyMatch
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			log.Error().Str("branch", branch).Str("synth_path", synthPath).
				Msg("hypothesize: nil store service; methodology disabled")
			return
		}
		f, err := svc.Search().GetByPath(ctx, branch, synthPath)
		if err != nil {
			log.Warn().Err(err).Str("branch", branch).Str("synth_path", synthPath).
				Msg("hypothesize: synth fact lookup failed; methodology section skipped")
			return
		}
		if f == nil {
			return
		}
		var mErr error
		matches, mErr = svc.Search().RelevantMethodologyForFact(
			ctx, branch,
			f.Path, f.Domain, f.Entities,
			3, ri.MethodologyMinScore(),
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
