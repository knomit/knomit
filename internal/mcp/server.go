// Package mcp implements the knomit MCP server.
package mcp

import (
	"github.com/mark3labs/mcp-go/server"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// NewServer creates a new MCP server with all knomit tools registered.
// If embedders are non-nil, the learn tool uses them for batch dedup embedding.
func NewServer(ri *repos.RepoInstance, reviewer *synthesize.Reviewer, profile string, embedders ...store.BatchEmbedder) *server.MCPServer {
	ontologyRoot := ri.OntologyRoot()
	ontology := ri.Ontology()

	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile, ontologyRoot, ontology)),
	)

	s.AddTool(learnTool(), LearnHandler(ri, embedders...))
	s.AddTool(queryTool(), QueryHandler(ri))
	s.AddTool(explainTool(), ExplainHandler(ri))
	s.AddTool(updateTool(), UpdateHandler(ri))
	s.AddTool(exploreTool(ontologyRoot), ExploreHandler(ri))
	s.AddTool(retractTool(), RetractHandler(ri))
	s.AddTool(hypothesizeTool(), HypothesizeHandler(ri))

	if reviewer != nil {
		s.AddTool(reviewTool(), ReviewHandler(reviewer))
	}

	return s
}
