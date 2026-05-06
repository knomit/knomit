package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"knomit/internal/fact"
	factpkg "knomit/internal/fact"
	"knomit/internal/repos"

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
			mcpgo.Description("A short label for this update moment."),
		),
		mcpgo.WithObject("updates",
			mcpgo.Required(),
			mcpgo.Description("Fields to update. Include only the fields you want to change."),
			mcpgo.Properties(map[string]any{
				"title":      map[string]any{"type": "string", "description": "New title."},
				"body":       map[string]any{"type": "string", "description": "New body text."},
				"type":       map[string]any{"type": "string", "description": "Epistemic type: observation, concept, process, principle, pattern, reference, synthesis, hypothesis, or methodology."},
				"confidence": map[string]any{"type": "number", "description": "Certainty level 0.0–1.0."},
				"sources":    map[string]any{"type": "integer", "description": "Number of independent sources."},
				"domain":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces domain tags."},
				"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces entity list."},
				"refs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Appended to existing refs."},
			}),
		),
	)
}

// updateInput represents the updates object in the request.
type updateInput struct {
	Type       *string  `json:"type"`
	Confidence *float64 `json:"confidence"`
	Sources    *int     `json:"sources"`
	Body       *string  `json:"body"`
	Title      *string  `json:"title"`
	Refs       []string `json:"refs"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
}

// UpdateHandler returns the handler function for knomit_update.
func UpdateHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		ri := repos.RepoFromContext(ctx)
		s := storeIndices(ri)
		agentBranch := ri.AgentBranch()
		ontologyRoot := ri.OntologyRoot()

		// 1. Get arguments.
		file := req.GetString("file", "")
		if file == "" {
			return mcpgo.NewToolResultError("file is required"), nil
		}
		file = factpkg.NormalizePath(ontologyRoot, file)
		momentName := req.GetString("moment_name", "")
		if momentName == "" {
			return mcpgo.NewToolResultError("moment_name is required"), nil
		}

		// 3. Check file exists.
		exists, err := s.facts.FactExists(ctx, agentBranch, file)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("file exists check error: %v", err)), nil
		}
		if !exists {
			return mcpgo.NewToolResultError(fmt.Sprintf("file not found: %s", file)), nil
		}

		// 4. Read and parse existing fact.
		readResult, err := s.facts.ReadFact(ctx, agentBranch, file, nil)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("read file error: %v", err)), nil
		}
		content := readResult.Content
		fact, err := fact.ParseFact(file, content)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("parse fact error: %v", err)), nil
		}

		// 5. Parse updates.
		var updates updateInput
		if err := unmarshalArg(req, "updates", &updates); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
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
		serialized, err := factpkg.SerializeFact(fact)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("serialize error: %v", err)), nil
		}
		commitMsg := fmt.Sprintf("update: %s", fact.Title)
		writeRes, err := s.facts.WriteFact(ctx, agentBranch, file, serialized, commitMsg, "update")
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("write error: %v", err)), nil
		}

		result := map[string]interface{}{
			"commit": writeRes.CommitHash,
		}
		out, err := json.Marshal(result)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}
