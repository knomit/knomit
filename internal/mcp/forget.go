package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// forgetTool returns the Tool definition for knomit_forget.
func forgetTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_forget",
		mcpgo.WithDescription("Delete a fact from the knowledge base."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file to delete."),
		),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this forget moment (used as a git tag)."),
		),
	)
}

// ForgetHandler returns the handler function for knomit_forget.
func ForgetHandler(gs GitStore, idx SearchIndex) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
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
		commitMsg := fmt.Sprintf("forget(%s): %s", momentName, file)
		if err := gs.DeleteFile(file, commitMsg); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("delete error: %v", err)), nil
		}

		// 5. Get commit hash.
		hash, err := gs.HeadCommit()
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("head commit error: %v", err)), nil
		}

		// 6. Delete from index.
		if err := idx.Delete(file); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("index delete error: %v", err)), nil
		}

		// 7. Tag.
		sanitized := sanitizeMomentName(momentName)
		tagName := "forget/" + sanitized
		if err := gs.Tag(tagName); err != nil {
			tagName = fmt.Sprintf("forget/%s-%d", sanitized, time.Now().Unix())
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
