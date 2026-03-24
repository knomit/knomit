package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"knomit/internal/fact"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// retractTool returns the Tool definition for knomit_retract.
func retractTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_retract",
		mcpgo.WithDescription("Retract a fact from the knowledge base."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file to retract."),
		),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this retraction moment."),
		),
	)
}

// RetractHandler returns the handler function for knomit_retract.
func RetractHandler(gs GitStore, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// 1. Get arguments.
		file := req.GetString("file", "")
		if file == "" {
			return mcpgo.NewToolResultError("file is required"), nil
		}
		file = fact.NormalizePath(ontologyRoot, file)
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}

		// 3. Check file exists.
		exists, err := gs.FileExists(file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("file exists check error: %v", err)), nil
		}
		if !exists {
			return mcpgo.NewToolResultError(fmt.Sprintf("file not found: %s", file)), nil
		}

		// 4. Delete the file.
		commitMsg := fmt.Sprintf("retract(%s): %s", momentName, file)
		hash, err := gs.DeleteFile(file, commitMsg, "retract")
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("delete error: %v", err)), nil
		}

		result := map[string]interface{}{
			"file":   file,
			"commit": hash,
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
