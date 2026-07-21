package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/repos"
	"knomit/internal/synthesize"
)

// HypothesizeResult is the JSON response returned by the hypothesize tool.
//
// Deliberately narrower than ReviewResult: there is no `summary`. A hypothesize
// session mutates nothing itself — the agent writes any hypothesis through
// knomit_learn — so the engine's prune/merge/synthesize counters describe work
// this tool did not do. Leaving them unmapped is the point; do not grow a
// summary field here without a counter that means something for this tool.
type HypothesizeResult struct {
	SessionID string               `json:"session_id"`
	Item      *HypothesizeItem     `json:"item,omitempty"`
	Done      bool                 `json:"done"`
	Progress  *HypothesizeProgress `json:"progress,omitempty"`
}

// HypothesizeItem describes a single synthesis fact to evaluate for hypothesis generation.
type HypothesizeItem struct {
	// ID identifies this specific work item. Clients should echo it back as
	// `item_id` on the continue call so the server can verify the response
	// belongs to the item that was rendered. Additive and optional — omitting
	// it answers whatever item is current, the pre-D2 behaviour.
	ID           int64           `json:"id"`
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
		mcpgo.WithNumber("item_id", mcpgo.Description("Echo back item.id from the work item you are answering. Optional but strongly recommended: it lets the server reject a response aimed at a stale item instead of applying it to a different one.")),
		mcpgo.WithString("effort", mcpgo.Description("Discovery effort dial: 'normal' (default), 'medium', or 'high'. Medium/high engage the structural-bridge engine for emergent keystone-hypothesis discovery (backward direction).")),
		mcpgo.WithArray("domain", mcpgo.Description("Optional scope filter: restrict the synthesis-fact seed pool to these domains. Empty = whole corpus.")),
		mcpgo.WithArray("entities", mcpgo.Description("Optional scope filter: restrict the synthesis-fact seed pool to facts tagged with these entities. Empty = whole corpus.")),
	)
}

// HypothesizeHandler returns the handler function for knomit_hypothesize.
//
// The handler is a thin shell over synthesize's shared pipeline engine: it
// resolves effort/scope, constructs a per-call Hypothesizer, and projects the
// engine's tool-neutral result onto this file's wire types. All session
// mechanics — seed scan, watermark gate, claim protocol, completion — live in
// internal/synthesize (pipeline.go + hypothesize_strategy.go).
func HypothesizeHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		b := repos.BindingFromContext(ctx)
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"read-only view: branch %q is not writable; facts are authored on %q",
				b.WriteMountBranch(), b.Write().AgentBranch())), nil
		}
		ri := b.Write()

		// Hold the mount's store for the duration of the call so a concurrent
		// SwapStore/Archive cannot close the SQLite handle mid-session. The
		// engine re-resolves indices from ri per operation; this acquisition is
		// what keeps those resolutions pointing at a live Service.
		_, release, err := storeIndices(ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer release()

		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		var result *synthesize.PipelineResult

		if sessionID == "" {
			effort, scope, perr := parseEffortAndScope(req, ri)
			if perr != nil {
				return mcpgo.NewToolResultError(perr.Error()), nil
			}
			result, err = synthesize.NewHypothesizer(ri, logProgress, effort, scope).StartSession(ctx)
		} else {
			// Effort and scope are deliberately NOT parsed on the continue path:
			// an invalid effort must not be able to wedge a live session, and the
			// scope that matters was persisted on the session row at start.
			result, err = synthesize.NewHypothesizer(ri, logProgress, synthesize.DefaultEffort, synthesize.ScopeFilter{}).
				ContinueSessionForItem(ctx, sessionID, response, int64(req.GetFloat("item_id", 0)))
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("hypothesize error: %v", err)), nil
		}

		resultJSON, _ := json.MarshalIndent(hypothesizeResult(result), "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}

// hypothesizeResult converts the engine's tool-neutral turn result into the
// hypothesize wire shape.
//
// Two mappings are worth naming. The item's raw stored payload becomes the
// `fact` field — hypothesize is the tool that ships its payload to the agent
// verbatim, which is what PipelineItem.FactsJSON exists for. And the engine's
// Summary is dropped: see HypothesizeResult.
func hypothesizeResult(res *synthesize.PipelineResult) *HypothesizeResult {
	if res == nil {
		return nil
	}
	out := &HypothesizeResult{
		SessionID: res.SessionID,
		Done:      res.Done,
	}
	if res.Progress != nil {
		out.Progress = &HypothesizeProgress{
			Completed: res.Progress.Completed,
			Remaining: res.Progress.Remaining,
		}
	}
	if res.Item != nil {
		out.Item = &HypothesizeItem{
			ID:           res.Item.ID,
			Type:         res.Item.Type,
			Instructions: res.Item.Prompt,
		}
		if res.Item.FactsJSON != "" {
			out.Item.Fact = json.RawMessage(res.Item.FactsJSON)
		}
	}
	return out
}
