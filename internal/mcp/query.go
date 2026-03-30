package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// queryTool returns the Tool definition for knomit_query.
func queryTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_query",
		mcpgo.WithDescription("Search the knowledge base. At least one of text, entities, domain, path, or min_confidence is required."),
		mcpgo.WithString("text",
			mcpgo.Description("Full-text search query."),
		),
		mcpgo.WithArray("entities",
			mcpgo.Description("Filter by entities (all must be present)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("domain",
			mcpgo.Description("Filter by domain tags."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithString("path",
			mcpgo.Description("Filter by path prefix."),
		),
		mcpgo.WithNumber("min_confidence",
			mcpgo.Description("Minimum confidence threshold (0–1)."),
		),
	)
}

// QueryHandler returns the handler function for knomit_query.
func QueryHandler(gs GitStore, idx SearchIndex, agentBranch string) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// 1. Build query.
		text := req.GetString("text", "")
		entities := req.GetStringSlice("entities", nil)
		domain := req.GetStringSlice("domain", nil)
		path := req.GetString("path", "")
		minConfidence := req.GetFloat("min_confidence", 0)

		// Validate at least one filter.
		if text == "" && len(entities) == 0 && len(domain) == 0 && path == "" && minConfidence == 0 {
			return mcpgo.NewToolResultError("at least one of text, entities, domain, path, or min_confidence is required"), nil
		}

		q := SearchQuery{
			Text:          text,
			Entities:      entities,
			Domain:        domain,
			Path:          path,
			MinConfidence: minConfidence,
			Limit:         20,
		}

		// 4. Search.
		results, err := idx.Search(ctx, agentBranch, q)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("search error: %v", err)), nil
		}

		// 5. Build output.
		type factOutput struct {
			File        string      `json:"file"`
			Title       string      `json:"title"`
			Type        string      `json:"type"`
			Body        string      `json:"body"`
			LastModified string     `json:"last_modified,omitempty"`
			Commit      string      `json:"commit"`
			Frontmatter interface{} `json:"frontmatter"`
		}
		type frontmatterOutput struct {
			Domain         []string `json:"domain"`
			Confidence     float64  `json:"confidence"`
			Sources        int      `json:"sources"`
			Entities       []string `json:"entities"`
			Refs           []string `json:"refs"`
			EvidenceWeight float64  `json:"evidence_weight,omitempty"`
		}

		facts := make([]factOutput, len(results))
		for i, r := range results {
			fm := frontmatterOutput{
				Domain:         orEmpty(r.Domain),
				Confidence:     r.Confidence,
				Sources:        r.Sources,
				Entities:       orEmpty(r.Entities),
				Refs:           orEmpty(r.Refs),
				EvidenceWeight: r.EvidenceWeight,
			}
			facts[i] = factOutput{
				File:        r.Path,
				Title:       r.Title,
				Type:        r.Type,
				Body:        r.Body,
				Commit:      r.CommitHash,
				Frontmatter: fm,
			}
		}

		out, err := json.Marshal(map[string]interface{}{"facts": facts})
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(out)), nil
	}
}

// orEmpty returns the slice or an empty slice if nil.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
