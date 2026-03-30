package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// ReviewResult is returned from StartSession and ContinueSession.
type ReviewResult struct {
	SessionID string          `json:"session_id"`
	Item      *ReviewItem     `json:"item,omitempty"`
	Done      bool            `json:"done,omitempty"`
	Summary   *ReviewStats    `json:"summary,omitempty"`
	Progress  *ReviewProgress `json:"progress,omitempty"`
}

// ReviewItem describes a single work item for the hosting model.
type ReviewItem struct {
	Type           string `json:"type"` // "prune" or "distill"
	Prompt         string `json:"prompt"`
	ResponseSchema string `json:"response_schema"`
}

// ReviewProgress tracks completed/remaining counts.
type ReviewProgress struct {
	Completed int `json:"completed"`
	Remaining int `json:"remaining"`
}

// ReviewStats tracks what actions were taken during a review.
type ReviewStats struct {
	Pruned      int
	Merged      int
	Updated     int
	Synthesized int
}

// Reviewer is the interface the review MCP tool requires from the synthesize
// package. Using an interface breaks the import cycle (synthesize imports mcp).
type Reviewer interface {
	StartSession(ctx context.Context) (*ReviewResult, error)
	ContinueSession(ctx context.Context, sessionID, response string) (*ReviewResult, error)
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
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")

		var result *ReviewResult
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
