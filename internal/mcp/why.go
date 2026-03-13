package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// whyTool returns the Tool definition for knomit_why.
func whyTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_why",
		mcpgo.WithDescription("Show the history and learning moment for a fact file."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file (e.g. know/topic/fact.md)."),
		),
	)
}

// WhyHandler returns the handler function for knomit_why.
func WhyHandler(gs GitStore, ontologyRoot string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// 1. Sync.
		if _, err := gs.Sync(nil); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("sync error: %v", err)), nil
		}

		// 2. Get file argument.
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
				// Find first tag starting with "learn/".
				var learnTag string
				var learnDate string
				for _, tag := range tags {
					if strings.HasPrefix(tag, "learn/") {
						learnTag = tag
						// Use the oldest commit's date.
						learnDate = oldestEntry.Date
						break
					}
				}
				if learnTag != "" {
					learningMoment = map[string]interface{}{
						"tag":      learnTag,
						"date":     learnDate,
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
			"fact":             factOut,
			"learning_moment":  learningMoment,
			"refs":             orEmpty(fact.Refs),
			"history":          history,
		}

		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
