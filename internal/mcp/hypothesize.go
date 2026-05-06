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
		mcpgo.WithDescription("Generate hypotheses from synthesis facts. Call with no arguments to start a new session. Call with session_id to continue processing the next fact."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your response/acknowledgement for the previous work item.")),
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

		var result *HypothesizeResult
		var err error

		if sessionID == "" {
			result, err = hypothesizeStart(ctx, ri, s, agentBranch)
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
func hypothesizeStart(ctx context.Context, ri *repos.RepoInstance, s mcpStore, agentBranch string) (*HypothesizeResult, error) {
	branch := agentBranch

	// Get watermark.
	watermark, err := s.pipeline.GetPipelineWatermark(ctx, "hypothesize", branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	var synthFacts []fact.Fact

	if watermark == "" {
		// First run: search for all synthesis facts.
		results, err := s.search.Search(ctx, agentBranch, store.SearchQuery{
			IncludeTypes: []string{"synthesis"},
			Limit:        100000,
		})
		if err != nil {
			return nil, fmt.Errorf("search synthesis facts: %w", err)
		}
		for _, r := range results {
			sf := fact.NewFact(r.Path)
			sf.Title = r.Title
			sf.Body = r.Body
			sf.Type = fact.EpistemicType(r.Type)
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

	// Create session.
	sess, err := s.pipeline.CreatePipelineSession(ctx, "hypothesize", branch)
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
	instructions := buildHypothesizeInstructions(ctx, ri, synthFact.Path())

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
// agent. When relevant methodology exists on the branch, it is appended to
// the instructions as an "Applicable methodology" section so the LLM sees
// the reasoning lessons inline rather than having to query for them.
func buildHypothesizeInstructions(ctx context.Context, ri *repos.RepoInstance, synthPath string) string {
	base := `1. Call knomit_explain on the synthesis fact to trace its provenance
2. If methodology candidates are listed below, scan their titles and scores. Fetch any that look relevant via knomit_query (single-path query). Within a session, you can rely on what you already fetched — don't re-query the same methodology twice.
3. Gather additional evidence as needed using knomit_query
4. Decide if a hypothesis is warranted based on the evidence
5. If yes, call knomit_learn with type: hypothesis, including: hypothesis statement, evidence chain, reasoning step, known gaps, falsification condition. If any methodology candidates listed below shaped your reasoning (after fetching the ones you found relevant via knomit_query), include their paths in your hypothesis's refs array. Cite only what you actually used.
6. After writing the hypothesis, call knomit_learn with type: methodology, topic: "meta", category: "reasoning" to record the reasoning process used. Set the methodology's domain and entities to the union of the source synthesis fact's tags plus the standard markers (meta, reasoning, methodology) — inherit from the source rather than inventing new tags.
7. Call knomit_hypothesize with session_id to continue to the next synthesis fact`

	// Retrieve relevant methodology and append as a structured section.
	section := loadMethodologySection(ctx, ri, synthPath)
	if section == "" {
		return base
	}
	return base + "\n\n" + section
}

// loadMethodologySection queries the branch's methodology for the given
// synthesis fact and renders it as a prompt-ready section. Returns "" when
// no methodology is relevant or any lookup fails.
func loadMethodologySection(ctx context.Context, ri *repos.RepoInstance, synthPath string) string {
	var matches []store.MethodologyMatch
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		// Read the synthesis fact to get its body+tags as the source.
		f, err := svc.Search().GetByPath(ctx, ri.AgentBranch(), synthPath)
		if err != nil || f == nil {
			return
		}
		matches, _ = svc.Search().RelevantMethodology(
			ctx, ri.AgentBranch(),
			f.Body, f.Domain, f.Entities, 3,
		)
	})
	return store.FormatMethodologySection(matches, ri.MethodologyMinScore())
}
