package mcp

import (
	"context"
	"encoding/json"
	"fmt"

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
//
// Starting a new session can take 60-120s on a large knowledge base (seed scan
// plus, at medium/high effort, clustering and structural-bridge building), so
// the tool advertises optional task support — same reasoning as knomit_review.
// Clients implementing the MCP tasks capability run the call asynchronously and
// poll tasks/get for completion, avoiding their tool-call timeout. Clients
// without task support get the original synchronous behavior.
func hypothesizeTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_hypothesize",
		mcpgo.WithDescription("Generate NEW hypothesis facts from synthesis facts on the agent branch. This is a distinct operation from knomit_review — only invoke when the user has explicitly asked to hypothesize, generate predictions, or extend synthesis facts forward. Do NOT invoke as a follow-up to knomit_review or other maintenance tools without an explicit user request. Each work item presents one synthesis fact; the agent decides per-item whether to write a hypothesis (skipping is the expected outcome for most synth facts — see workflow). Call with no arguments to start a new session. Call with session_id to continue processing the next fact. STOP when the result has done: true — the session is finished and that session_id can no longer be continued; calling with it again is an error. A start call may return done: true immediately when no synthesis facts are eligible; that is a normal, complete outcome, not a reason to retry."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your response/acknowledgement for the previous work item.")),
		mcpgo.WithNumber("item_id", mcpgo.Description("Echo back item.id from the work item you are answering. Optional but strongly recommended: it lets the server reject a response aimed at a stale item instead of applying it to a different one.")),
		mcpgo.WithString("effort", mcpgo.Description("Discovery effort dial: 'normal' (default), 'medium', or 'high'. Medium/high engage the structural-bridge engine for emergent keystone-hypothesis discovery (backward direction).")),
		mcpgo.WithArray("domain", mcpgo.Description("Optional scope filter: restrict the synthesis-fact seed pool to these domains. Empty = whole corpus.")),
		mcpgo.WithArray("entities", mcpgo.Description("Optional scope filter: restrict the synthesis-fact seed pool to facts tagged with these entities. Empty = whole corpus.")),
		mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional),
	)
}

// HypothesizeHandler returns the handler function for knomit_hypothesize.
//
// The handler is a thin shell over synthesize's shared pipeline engine: it
// resolves effort/scope, constructs a per-call Hypothesizer, and projects the
// engine's tool-neutral result onto this file's wire types. All session
// mechanics — seed scan, watermark gate, claim protocol, completion — live in
// internal/synthesize (pipeline.go + hypothesize_strategy.go).
//
// The handler signature is unchanged whether the call arrives synchronously or
// wrapped as a task — mcp-go dispatches it appropriately based on the client's
// request shape.
//
// When invoked as a task, mcp-go runs the handler in a goroutine but passes the
// HTTP request context, which Go's net/http cancels as soon as the initial
// CreateTaskResult response is sent. Without detaching, our work would see
// context.Canceled on the first SQL query. context.WithoutCancel keeps the
// values (notably the repo) but suppresses the cancellation that comes from the
// request lifecycle ending; client-initiated cancellation via tasks/cancel
// still works because mcp-go uses a separate cancel func.
func HypothesizeHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if req.Params.Task != nil {
			ctx = context.WithoutCancel(ctx)
		}
		// See the identical guard in ReviewHandler (knomit#122). This tool
		// shares parseEffortAndScope and therefore the same silent-drop
		// exposure: an unrecognised scope key runs the pass whole-corpus.
		if err := rejectUnknownArguments(req, hypothesizeTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		b := repos.BindingFromContext(ctx)
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"read-only view: branch %q is not writable; facts are authored on %q",
				b.WriteMountBranch(), b.Write().AgentBranch())), nil
		}
		ri := b.Write()

		// Pin the write mount's store for the whole hypothesize call: the engine
		// re-resolves store indices from ri per operation and uses them across a
		// long LLM-driven session step, so without the pin a concurrent
		// SwapStore/Archive could close the SQLite handle mid-session. The pin
		// makes those drains wait for this call instead. Pin (rather than
		// storeIndices) because nothing here needs the indices themselves.
		unpin, err := ri.Pin()
		if err != nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		defer unpin()

		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		var result *synthesize.PipelineResult

		if sessionID == "" {
			effort, scope, perr := parseEffortAndScope(req, ri)
			if perr != nil {
				return mcpgo.NewToolResultError(perr.Error()), nil
			}
			// Attribute the session this call is about to open (knomit#123).
			// Only the start path needs it: StartSession is the sole reader.
			result, err = synthesize.NewHypothesizer(ri, logProgress, effort, scope).
				StartSession(withActor(ctx, req))
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
