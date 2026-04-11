// Package mcp implements the knomit MCP server.
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// NewServer creates a profile-scoped MCP server with all knomit tools registered.
// The server is shared across all repos — each tool handler resolves the repo
// from the request context at call time. Instructions are computed per-session
// using the repo in the initialize request's context.
//
// defaultOntologyRoot is the config-level ontology root used only for the
// static exploreTool description (the actual default path at request time
// comes from ri.OntologyRoot()).
func NewServer(profile, defaultOntologyRoot string, embedders ...store.BatchEmbedder) *server.MCPServer {
	hooks := &server.Hooks{}
	hooks.AddAfterInitialize(func(ctx context.Context, id any, req *mcp.InitializeRequest, result *mcp.InitializeResult) {
		ri, ok := repos.RepoFromContextOpt(ctx)
		if !ok {
			// No repo in ctx — fall back to generic instructions.
			result.Instructions = ProfileInstructions(profile, defaultOntologyRoot, nil)
			return
		}
		result.Instructions = ProfileInstructions(profile, ri.OntologyRoot(), ri.Ontology())
	})

	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithHooks(hooks),
	)

	s.AddTool(learnTool(), LearnHandler(embedders...))
	s.AddTool(queryTool(), QueryHandler())
	s.AddTool(explainTool(), ExplainHandler())
	s.AddTool(updateTool(), UpdateHandler())
	s.AddTool(exploreTool(defaultOntologyRoot), ExploreHandler())
	s.AddTool(retractTool(), RetractHandler())
	s.AddTool(hypothesizeTool(), HypothesizeHandler())
	s.AddTool(reviewTool(), ReviewHandler())

	return s
}
