package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// HypothesizeResult is the JSON response returned by the hypothesize tool.
type HypothesizeResult struct {
	SessionID string              `json:"session_id"`
	Item      *HypothesizeItem    `json:"item,omitempty"`
	Done      bool                `json:"done"`
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
func HypothesizeHandler(gs GitStore, idx SearchIndex, pipelineIdx PipelineIndex, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		var result *HypothesizeResult
		var err error

		if sessionID == "" {
			result, err = hypothesizeStart(gs, idx, pipelineIdx, ontologyRoot)
		} else {
			result, err = hypothesizeContinue(pipelineIdx, gs, ontologyRoot, sessionID, response)
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("hypothesize error: %v", err)), nil
		}

		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}

// hypothesizeStart creates a new session, finds synthesis facts, and returns the first item.
func hypothesizeStart(gs GitStore, idx SearchIndex, pipelineIdx PipelineIndex, ontologyRoot string) (*HypothesizeResult, error) {
	branch := gs.Branch()

	// GC old sessions.
	_ = pipelineIdx.GCPipelineSessions("hypothesize", branch, 5)

	// Get watermark.
	watermark, err := pipelineIdx.GetPipelineWatermark("hypothesize", branch)
	if err != nil {
		return nil, fmt.Errorf("get watermark: %w", err)
	}

	var synthFacts []Fact

	if watermark == "" {
		// First run: search for all synthesis facts.
		results, err := idx.Search(SearchQuery{
			IncludeTypes: []string{"synthesis"},
			Limit:        100000,
		})
		if err != nil {
			return nil, fmt.Errorf("search synthesis facts: %w", err)
		}
		for _, r := range results {
			synthFacts = append(synthFacts, Fact{
				Path:       r.Path,
				Title:      r.Title,
				Body:       r.Body,
				Type:       fact.EpistemicType(r.Type),
				Domain:     r.Domain,
				Confidence: r.Confidence,
				Sources:    r.Sources,
				Entities:   r.Entities,
			})
		}
	} else {
		// Incremental: find changed files since watermark.
		added, modified, _, err := gs.DiffFiles(watermark)
		if err != nil {
			return nil, fmt.Errorf("diff files: %w", err)
		}
		changedPaths := append(added, modified...)
		for _, p := range changedPaths {
			if !strings.HasSuffix(p, ".md") {
				continue
			}
			content, readErr := gs.ReadFile(p)
			if readErr != nil {
				continue
			}
			f, parseErr := ParseFact(p, content)
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
		if head, err := gs.HeadCommit(); err == nil {
			_ = pipelineIdx.SetPipelineWatermark("hypothesize", branch, head)
		}
		return &HypothesizeResult{Done: true}, nil
	}

	// Create session.
	sess, err := pipelineIdx.CreatePipelineSession("hypothesize", branch)
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
		if err := pipelineIdx.InsertPipelineWorkItem(item); err != nil {
			return nil, fmt.Errorf("insert work item: %w", err)
		}
	}

	return hypothesizeNextItem(pipelineIdx, gs, ontologyRoot, sess.ID)
}

// hypothesizeContinue acknowledges the current work item and advances to the next.
func hypothesizeContinue(pipelineIdx PipelineIndex, gs GitStore, ontologyRoot, sessionID, response string) (*HypothesizeResult, error) {
	// Verify session exists and is active.
	sess, err := pipelineIdx.GetPipelineSession(sessionID)
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
	current, err := pipelineIdx.NextPipelineWorkItem(sessionID)
	if err != nil {
		return nil, fmt.Errorf("get current item: %w", err)
	}
	if current != nil {
		resp := response
		if resp == "" {
			resp = "acknowledged"
		}
		if err := pipelineIdx.SetPipelineWorkItemResponse(current.ID, resp); err != nil {
			return nil, fmt.Errorf("set response: %w", err)
		}
	}

	return hypothesizeNextItem(pipelineIdx, gs, ontologyRoot, sessionID)
}

// hypothesizeNextItem fetches the next unanswered work item or completes the session.
func hypothesizeNextItem(pipelineIdx PipelineIndex, gs GitStore, ontologyRoot, sessionID string) (*HypothesizeResult, error) {
	item, err := pipelineIdx.NextPipelineWorkItem(sessionID)
	if err != nil {
		return nil, fmt.Errorf("next item: %w", err)
	}

	// No more items → complete session and advance watermark.
	if item == nil {
		if err := pipelineIdx.CompletePipelineSession(sessionID); err != nil {
			return nil, fmt.Errorf("complete session: %w", err)
		}
		branch := gs.Branch()
		if head, err := gs.HeadCommit(); err == nil {
			_ = pipelineIdx.SetPipelineWatermark("hypothesize", branch, head)
		}
		return &HypothesizeResult{
			SessionID: sessionID,
			Done:      true,
		}, nil
	}

	// Build instructions.
	instructions := buildHypothesizeInstructions(ontologyRoot)

	completed, remaining, _ := pipelineIdx.PipelineWorkItemStats(sessionID)

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

// buildHypothesizeInstructions returns the step-by-step instructions for the agent.
func buildHypothesizeInstructions(ontologyRoot string) string {
	return fmt.Sprintf(`1. Query %s/meta/reasoning/ with domain/entity filters from the synthesis fact for applicable methodology
2. Call knomit_explain on the synthesis fact to trace its provenance
3. Gather additional evidence as needed using knomit_query
4. Decide if a hypothesis is warranted based on the evidence
5. If yes, call knomit_learn with type: hypothesis, including: hypothesis statement, evidence chain, reasoning step, known gaps, falsification condition
6. Call knomit_hypothesize with session_id to continue to the next synthesis fact`, ontologyRoot)
}

