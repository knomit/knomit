package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
			mcpgo.Description("A short label for this retraction moment (used as a git tag)."),
		),
	)
}

// RetractHandler returns the handler function for knomit_retract.
func RetractHandler(gs GitStore, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// 1. Sync.
		if _, err := gs.Sync(nil); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("sync error: %v", err)), nil
		}

		// 2. Get arguments.
		file := req.GetString("file", "")
		if file == "" {
			return mcpgo.NewToolResultError("file is required"), nil
		}
		file = normalizePath(ontologyRoot, file)
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
		hash, err := gs.DeleteFile(file, commitMsg)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("delete error: %v", err)), nil
		}

		// 5. Tag.
		sanitized := sanitizeMomentName(momentName)
		tagName := "retract/" + sanitized
		if err := gs.Tag(tagName); err != nil {
			tagName = fmt.Sprintf("retract/%s-%d", sanitized, time.Now().Unix())
			if err2 := gs.Tag(tagName); err2 != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("tag error: %v", err2)), nil
			}
		}

		result := map[string]interface{}{
			"file":       file,
			"commit":     hash,
			"moment_tag": tagName,
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
