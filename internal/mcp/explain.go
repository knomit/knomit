package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// explainTool returns the Tool definition for knomit_explain.
func explainTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_explain",
		mcpgo.WithDescription("Show the history and learning moment for a fact file."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file (e.g. general/technology/languages/go/abc123.md)."),
		),
	)
}

// ExplainHandler returns the handler function for knomit_explain.
func ExplainHandler(gs GitStore, sessionIdx ToolSessionIndex, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		_ = sessionIdx // will be used in Task 4
		// 1. Get file argument.
		file := req.GetString("file", "")
		if file == "" {
			return mcpgo.NewToolResultError("file is required"), nil
		}
		file = normalizePath(ontologyRoot, file)

		// 3. Read and parse the fact file.
		content, err := gs.ReadFile(file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("read file error: %v", err)), nil
		}
		fact, err := ParseFact(file, content)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("parse fact error: %v", err)), nil
		}

		// 4. Get commit log for this file.
		logEntries, err := gs.Log(file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("log error: %v", err)), nil
		}

		// 5. Find oldest commit hash.
		var learningMoment interface{}
		if len(logEntries) > 0 {
			oldestEntry := logEntries[len(logEntries)-1]
			tags, tagErr := gs.TagsContaining(oldestEntry.Commit)
			if tagErr == nil {
				// Find first known knowledge tag.
				var knownTag string
				var knownDate string
				for _, tag := range tags {
					if strings.HasPrefix(tag, "learn/") || strings.HasPrefix(tag, "update/") || strings.HasPrefix(tag, "subsume/") || strings.HasPrefix(tag, "retract/") {
						knownTag = tag
						knownDate = oldestEntry.Date
						break
					}
				}
				if knownTag != "" {
					learningMoment = map[string]interface{}{
						"tag":      knownTag,
						"date":     knownDate,
						"siblings": []interface{}{},
					}
				}
			}
		}

		// 6. Build history output.
		type historyEntry struct {
			Commit  string `json:"commit"`
			Date    string `json:"date"`
			Message string `json:"message"`
		}
		history := make([]historyEntry, len(logEntries))
		for i, e := range logEntries {
			history[i] = historyEntry{
				Commit:  e.Commit,
				Date:    e.Date,
				Message: e.Message,
			}
		}

		// 7. Build fact output.
		factOut := map[string]interface{}{
			"file":  fact.Path,
			"title": fact.Title,
			"body":  fact.Body,
			"frontmatter": map[string]interface{}{
				"domain":     orEmpty(fact.Domain),
				"confidence": fact.Confidence,
				"sources":    fact.Sources,
				"entities":   orEmpty(fact.Entities),
				"refs":       orEmpty(fact.Refs),
			},
		}

		result := map[string]interface{}{
			"fact":            factOut,
			"learning_moment": learningMoment,
			"refs":            orEmpty(fact.Refs),
			"history":         history,
		}

		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
