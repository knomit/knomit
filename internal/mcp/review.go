package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/synthesize"
)

// reviewTool returns the Tool definition for knomit_review.
//
// Starting a new session can take 60-120s on a large knowledge base
// (clustering + dedup of dirty facts), so the tool advertises optional task
// support. Clients implementing the MCP tasks capability will run the call
// asynchronously and poll tasks/get for completion, avoiding their tool-call
// timeout. Clients without task support get the original synchronous behavior.
func reviewTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_review",
		mcpgo.WithDescription("Maintain the existing knowledge base: prune redundant facts, distill clusters into higher-order synthesis facts, and reflect on hypothesis transitions to record methodology. Does NOT generate new hypotheses — that is a separate explicit operation via knomit_hypothesize. When a user asks for a 'review', they want only this tool; do not chain to knomit_hypothesize unless the user explicitly requests hypothesis generation. Call with no arguments to start a new review session. Call with session_id + response to continue."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your JSON decisions for the previous work item.")),
		mcpgo.WithString("effort", mcpgo.Description("Discovery effort dial: 'normal' (default — pre-discovery behaviour), 'medium', or 'high'. Medium/high engage the structural-bridge engine to surface emergent synthesis facts from cross-cluster bridges.")),
		mcpgo.WithArray("domain", mcpgo.Description("Optional scope filter: restrict the seed pool to facts in these domains. Empty = whole corpus.")),
		mcpgo.WithArray("entities", mcpgo.Description("Optional scope filter: restrict the seed pool to facts tagged with these entities. Empty = whole corpus.")),
		mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional),
	)
}

// ReviewHandler returns the handler function for knomit_review.
// A fresh synthesize.Reviewer is constructed per call from the repo in ctx.
//
// The handler signature is unchanged whether the call arrives synchronously
// or wrapped as a task — mcp-go dispatches it appropriately based on the
// client's request shape.
//
// When invoked as a task, mcp-go runs the handler in a goroutine but passes
// the HTTP request context, which Go's net/http cancels as soon as the
// initial CreateTaskResult response is sent. Without detaching, our work
// would see context.Canceled on the first SQL query. context.WithoutCancel
// keeps the values (notably the repo) but suppresses the cancellation that
// comes from the request lifecycle ending; client-initiated cancellation via
// tasks/cancel still works because mcp-go uses a separate cancel func.
func ReviewHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if req.Params.Task != nil {
			ctx = context.WithoutCancel(ctx)
		}
		ri := repos.RepoFromContext(ctx)

		effort := synthesize.Effort(req.GetString("effort", ""))
		if effort == "" {
			effort = synthesize.Effort(ri.DiscoveryEffortDefault())
		}
		if err := effort.Validate(); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		scope := synthesize.ScopeFilter{
			Domain:   req.GetStringSlice("domain", nil),
			Entities: req.GetStringSlice("entities", nil),
		}
		reviewer := synthesize.NewReviewerWithOptions(ri, logProgress, effort, scope)

		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		var result *synthesize.ReviewResult
		var err error

		if sessionID == "" {
			result, err = reviewer.StartSession(ctx)
		} else {
			if response == "" {
				return mcpgo.NewToolResultError("response is required when continuing a session"), nil
			}
			result, err = reviewer.ContinueSession(ctx, sessionID, response)
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("review error: %v", err)), nil
		}

		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}

// logProgress surfaces synthesize.ProgressEvent emissions to the server log.
// "warn" phases (e.g. validation rejections from ApplyDistillDecisions) go
// out at WARN; everything else at DEBUG so the server log stays usable
// during long review sessions.
func logProgress(e synthesize.ProgressEvent) {
	if e.Phase == "warn" {
		log.Warn().Str("phase", e.Phase).Msg(e.Message)
		return
	}
	log.Debug().Str("phase", e.Phase).Msg(e.Message)
}
