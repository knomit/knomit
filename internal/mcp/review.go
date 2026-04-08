package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/synthesize"
)

// reviewTool returns the Tool definition for knomit_review.
func reviewTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_review",
		mcpgo.WithDescription("Review and maintain the knowledge base. Call with no arguments to start a new review session. Call with session_id + response to continue."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your JSON decisions for the previous work item.")),
	)
}

// ReviewHandler returns the handler function for knomit_review.
func ReviewHandler(reviewer *synthesize.Reviewer) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

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
