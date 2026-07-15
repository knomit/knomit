package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
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
			mcpgo.Description("Fields to update. Include only the fields you want to change. origin and the topic/category path are immutable and not accepted here — fixing either requires knomit_retract plus a fresh knomit_learn."),
			mcpgo.Properties(map[string]any{
				"title":      map[string]any{"type": "string", "description": "New title."},
				"body":       map[string]any{"type": "string", "description": "New body text."},
				"kind":       map[string]any{"type": "string", "description": "Classification family — epistemic (descriptive) or pragmatic (prescriptive). Changing kind also requires a compatible type.", "enum": []string{"epistemic", "pragmatic"}},
				"type":       map[string]any{"type": "string", "description": "Leaf type. Epistemic: observation, concept, process, principle, pattern, reference, synthesis, insight, hypothesis, methodology. Pragmatic: policy, heuristic."},
				"confidence": map[string]any{"type": "number", "description": "Certainty level 0.0–1.0."},
				"sources":    map[string]any{"type": "integer", "description": "Number of independent sources."},
				"domain":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces domain tags."},
				"entities":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces entity list."},
				"refs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Replaces the ENTIRE refs list. Send every ref the fact should keep — any existing ref you leave out is dropped. To add or refresh a ref, read the current refs first and resend the full merged list. Omit the field to leave refs unchanged."},
			}),
		),
	)
}

// updateInput represents the updates object in the request.
type updateInput struct {
	Kind       *string  `json:"kind"`
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
		ontology := ri.Ontology()

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

		// 6. Merge updates into fact. (kind, type) validation is deferred
		// to SerializeFact below — it's the single source of truth for
		// kind/type consistency.
		if updates.Kind != nil {
			fact.Kind = factpkg.Kind(*updates.Kind)
		}
		if updates.Type != nil {
			fact.Type = factpkg.Type(*updates.Type)
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
		// Refs replace wholesale, like Domain and Entities — the caller
		// sends the complete new list. Dropping a ref only affects this
		// and future revisions: prior revisions keep their refs in git
		// history and their DERIVED_FROM edges in the graph. Deduped so
		// a careless caller can't accumulate duplicates in one call.
		if updates.Refs != nil {
			var refs []string
			for _, ref := range updates.Refs {
				refs = factpkg.AppendUnique(refs, ref)
			}
			fact.Refs = refs
		}

		// 7. Validate the assembled fact against the ontology's rules.
		// Derive topic/category by stripping the ontologyRoot prefix and
		// the final /<uuid>.md segment from the normalized fact path.
		if ontology != nil {
			topicCategory := strings.TrimPrefix(file, ontologyRoot+"/")
			topicCategory = path.Dir(topicCategory)
			if err := factpkg.ValidateFact(ontology, topicCategory, fact); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		}

		// 8. Write updated fact.
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
