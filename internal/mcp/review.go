package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"knomit/internal/clustercache"
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
		mcpgo.WithDescription("Review and maintain the knowledge base. Call with no arguments to start a new review session. Call with session_id + response to continue."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your JSON decisions for the previous work item.")),
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
//
// While the slow work runs, a heartbeat goroutine fires periodic
// notifications/message events with the latest phase from the synthesize
// layer so MCP clients can show progress and (depending on their
// implementation) avoid response timeouts. Heartbeat interval comes from
// config; zero disables it.
func ReviewHandler(srv *mcpserver.MCPServer, heartbeat time.Duration, cache *clustercache.Cache) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if req.Params.Task != nil {
			ctx = context.WithoutCancel(ctx)
		}
		ri := repos.RepoFromContext(ctx)

		// Capture the latest phase event from the synthesize layer so the
		// heartbeat can include it in the user-facing message.
		var latestPhase atomic.Pointer[string]
		var clusterFn synthesize.ClusterFn
		if cache != nil {
			clusterFn = synthesize.ClusterFn(cache.ClusterFnFor(ri))
		}
		reviewer := synthesize.NewReviewer(ri, clusterFn, func(e synthesize.ProgressEvent) {
			s := e.Phase
			if e.Message != "" {
				s += ": " + e.Message
			}
			latestPhase.Store(&s)
		})

		// Start the heartbeat goroutine. stop is closed in the deferred call
		// regardless of how the handler returns, so the goroutine never
		// outlives the request.
		stop := make(chan struct{})
		defer close(stop)
		if heartbeat > 0 {
			go runHeartbeat(srv, ctx, stop, &latestPhase, time.Now(), heartbeat)
		}

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

// runHeartbeat sends a notifications/message event to the client every
// `interval` until `stop` closes or `ctx` is done. The message includes
// elapsed time and the most recent phase string set by the synthesize
// onProgress callback. Errors are ignored — losing a heartbeat is fine.
func runHeartbeat(srv *mcpserver.MCPServer, ctx context.Context, stop <-chan struct{}, latest *atomic.Pointer[string], start time.Time, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			phase := "starting"
			if p := latest.Load(); p != nil && *p != "" {
				phase = *p
			}
			elapsed := time.Since(start).Round(time.Second)
			data := fmt.Sprintf("knomit_review: working (%s elapsed, phase: %s)", elapsed, phase)
			_ = srv.SendNotificationToClient(ctx, "notifications/message", map[string]any{
				"level":  "info",
				"logger": "knomit",
				"data":   data,
			})
		}
	}
}
