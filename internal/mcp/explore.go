package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const explorePageSize = 25

// exploreTool returns the Tool definition for knomit_explore.
func exploreTool(ontologyRoot string) mcpgo.Tool {
	return mcpgo.NewTool("knomit_explore",
		mcpgo.WithDescription("Browse knowledge base facts ordered by most recently updated. Returns paginated results. Call with no cursor to start; pass the returned cursor to get the next page. Use knomit_explain for history on individual facts."),
		mcpgo.WithString("path",
			mcpgo.Description(fmt.Sprintf("Filter to a subtree (default: %q).", ontologyRoot)),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Session ID from a previous call. Omit to start a new session."),
		),
	)
}

// ExploreHandler returns the handler function for knomit_explore.
func ExploreHandler(gs GitStore, sessionIdx ToolSessionIndex, ontologyRoot, agentBranch string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		path := req.GetString("path", ontologyRoot)
		cursor := req.GetString("cursor", "")

		var seen map[string]bool
		var fromCommit string

		if cursor == "" {
			// New session: GC old sessions, start fresh.
			_ = sessionIdx.GCToolSessions("explore", agentBranch, 5)
		} else {
			// Resume existing session.
			session, err := sessionIdx.GetToolSession(cursor)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("session lookup error: %v", err)), nil
			}
			if session == nil || session.Status != "active" {
				return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
			}
			seen, err = sessionIdx.GetSeenPaths(cursor)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("seen paths error: %v", err)), nil
			}
			fromCommit = session.LastCommit
		}

		files, lastCommit, err := gs.WalkChangedFiles(agentBranch, fromCommit, path, seen, explorePageSize)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("walk error: %v", err)), nil
		}

		type factOutput struct {
			Path    string `json:"path"`
			Title   string `json:"title"`
			Type    string `json:"type"`
			Updated string `json:"updated"`
		}

		var facts []factOutput
		var newPaths []string

		for _, f := range files {
			content, readErr := gs.ReadFile(agentBranch, f.Path)
			if readErr != nil {
				continue // deleted or unreadable — skip
			}
			parsed, parseErr := ParseFact(f.Path, content)
			if parseErr != nil {
				continue
			}
			facts = append(facts, factOutput{
				Path:    f.Path,
				Title:   parsed.Title,
				Type:    string(parsed.Type),
				Updated: f.Timestamp.Format(time.RFC3339),
			})
			newPaths = append(newPaths, f.Path)
		}

		// Empty KB on first call: return immediately without creating a session.
		if cursor == "" && len(facts) == 0 {
			out, _ := json.Marshal(map[string]interface{}{
				"facts":    []factOutput{},
				"cursor":   nil,
				"has_more": false,
			})
			return mcpgo.NewToolResultText(string(out)), nil
		}

		hasMore := len(files) >= explorePageSize

		// Create session on first call.
		var sessionID string
		if cursor == "" {
			session, err := sessionIdx.CreateToolSession("explore", agentBranch, path)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("create session error: %v", err)), nil
			}
			sessionID = session.ID
		} else {
			sessionID = cursor
		}

		// Record seen paths.
		if len(newPaths) > 0 {
			if err := sessionIdx.AddSeenPaths(sessionID, newPaths); err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
			}
		}

		// Update session with last commit and status.
		status := "active"
		if !hasMore {
			status = "completed"
		}
		if err := sessionIdx.UpdateToolSession(sessionID, lastCommit, status); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("update session error: %v", err)), nil
		}

		var cursorOut interface{} = sessionID
		if !hasMore {
			cursorOut = nil
		}

		out, err := json.Marshal(map[string]interface{}{
			"facts":    facts,
			"cursor":   cursorOut,
			"has_more": hasMore,
		})
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
