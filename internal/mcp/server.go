// Package mcp implements the knomit MCP server.
package mcp

import (
	"github.com/mark3labs/mcp-go/server"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// NewServer creates a new MCP server with all knomit tools registered.
// If embedder is non-nil, the learn tool uses it for batch dedup embedding.
func NewServer(gs store.FactIndex, idx store.SearchIndex, sessionIdx store.ToolSessionIndex, pipelineIdx store.PipelineIndex, branches store.BranchIndex, reviewer Reviewer, profile, ontologyRoot string, ontology *fact.Ontology, agentBranch string, embedders ...store.BatchEmbedder) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile, ontologyRoot, ontology)),
	)

	s.AddTool(learnTool(), LearnHandler(gs, idx, ontologyRoot, ontology, agentBranch, embedders...))
	s.AddTool(queryTool(), QueryHandler(gs, idx, agentBranch))
	s.AddTool(explainTool(), ExplainHandler(gs, idx, sessionIdx, ontologyRoot, agentBranch))
	s.AddTool(updateTool(), UpdateHandler(gs, ontologyRoot, agentBranch))
	s.AddTool(exploreTool(ontologyRoot), ExploreHandler(gs, idx, sessionIdx, ontologyRoot, agentBranch))
	s.AddTool(retractTool(), RetractHandler(gs, ontologyRoot, agentBranch))

	s.AddTool(hypothesizeTool(), HypothesizeHandler(gs, idx, pipelineIdx, branches, ontologyRoot, agentBranch))

	if reviewer != nil {
		s.AddTool(reviewTool(), ReviewHandler(reviewer))
	}

	return s
}
