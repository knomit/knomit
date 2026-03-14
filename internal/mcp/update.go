package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	factpkg "knomit/internal/fact"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// updateTool returns the Tool definition for knomit_update.
func updateTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_update",
		mcpgo.WithDescription("Update an existing fact in the knowledge base."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file to update."),
		),
		mcpgo.WithString("moment_name",
			mcpgo.Required(),
			mcpgo.Description("A short label for this update moment (used as a git tag)."),
		),
		mcpgo.WithObject("updates",
			mcpgo.Required(),
			mcpgo.Description("Fields to update."),
		),
	)
}

// updateInput represents the updates object in the request.
type updateInput struct {
	Type       *string   `json:"type"`
	Confidence *float64  `json:"confidence"`
	Sources    *int      `json:"sources"`
	Body       *string   `json:"body"`
	Title      *string   `json:"title"`
	Refs       []string  `json:"refs"`
	Domain     []string  `json:"domain"`
	Entities   []string  `json:"entities"`
}

// UpdateHandler returns the handler function for knomit_update.
func UpdateHandler(gs GitStore, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
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

		// 4. Read and parse existing fact.
		content, err := gs.ReadFile(file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("read file error: %v", err)), nil
		}
		fact, err := ParseFact(file, content)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("parse fact error: %v", err)), nil
		}

		// 5. Parse updates.
		args := req.GetArguments()
		updatesRaw, ok := args["updates"]
		if !ok {
			return mcpgo.NewToolResultError("updates is required"), nil
		}
		updatesJSON, err := json.Marshal(updatesRaw)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("invalid updates: %v", err)), nil
		}
		var updates updateInput
		if err := json.Unmarshal(updatesJSON, &updates); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("invalid updates format: %v", err)), nil
		}

		// 6. Merge updates into fact.
		if updates.Type != nil {
			eType := factpkg.EpistemicType(*updates.Type)
			if err := eType.Validate(); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			fact.Type = eType
		}
		if updates.Confidence != nil {
			fact.Confidence = *updates.Confidence
		}
		if updates.Sources != nil {
			fact.Sources = *updates.Sources
		}
		if updates.Body != nil {
			fact.Body = *updates.Body
		}
		if updates.Title != nil {
			fact.Title = *updates.Title
		}
		if updates.Domain != nil {
			fact.Domain = updates.Domain
		}
		if updates.Entities != nil {
			fact.Entities = updates.Entities
		}
		// Refs are appended (not replaced).
		if len(updates.Refs) > 0 {
			fact.Refs = append(fact.Refs, updates.Refs...)
		}

		// 7. Write updated fact.
		commitMsg := fmt.Sprintf("update: %s", fact.Title)
		hash, _, err := gs.WriteFile(file, SerializeFact(fact), commitMsg)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("write error: %v", err)), nil
		}

		// 8. Tag.
		sanitized := sanitizeMomentName(momentName)
		tagName := "update/" + sanitized
		if err := gs.Tag(tagName); err != nil {
			tagName = fmt.Sprintf("update/%s-%d", sanitized, time.Now().Unix())
			if err2 := gs.Tag(tagName); err2 != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("tag error: %v", err2)), nil
			}
		}

		result := map[string]interface{}{
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
