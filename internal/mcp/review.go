package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// Reviewer is the interface the review MCP tool requires from the synthesize
// package. Using an interface breaks the import cycle (synthesize imports mcp).
type Reviewer interface {
	StartSession() (interface{}, error)
	ContinueSession(sessionID, response string) (interface{}, error)
}

// reviewTool returns the Tool definition for knomit_review.
func reviewTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_review",
		mcpgo.WithDescription("Review and maintain the knowledge base. Call with no arguments to start a new review session. Call with session_id + response to continue."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your JSON decisions for the previous work item.")),
	)
}

// ReviewHandler returns the handler function for knomit_review.
func ReviewHandler(reviewer Reviewer) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		var result interface{}
		var err error

		if sessionID == "" {
			result, err = reviewer.StartSession()
		} else {
			if response == "" {
				return mcpgo.NewToolResultError("response is required when continuing a session"), nil
			}
			result, err = reviewer.ContinueSession(sessionID, response)
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("review error: %v", err)), nil
		}

		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}
