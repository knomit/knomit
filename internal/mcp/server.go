// Package mcp implements the knomit MCP server.
package mcp

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"knomit/internal/clustercache"
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
//
// heartbeat is the interval at which long-running tools (knomit_review) emit
// periodic notifications/message events so clients with response timeouts
// can see progress and avoid timing out. Zero disables the heartbeat.
//
// cache is the cluster-cache facade used by knomit_review to avoid
// recomputing Louvain on every call; may be nil in tests, in which case
// Reviewer falls back to direct ClusterFacts.
func NewServer(profile, defaultOntologyRoot string, heartbeat time.Duration, cache *clustercache.Cache, embedders ...store.BatchEmbedder) *server.MCPServer {
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
		// Advertise tasks capability so clients that support it can invoke
		// long-running tools (knomit_review) asynchronously and poll for
		// completion via tasks/get instead of blocking on a single response.
		server.WithTaskCapabilities(true, true, true),
	)

	s.AddTool(learnTool(), LearnHandler(embedders...))
	s.AddTool(queryTool(), QueryHandler())
	s.AddTool(explainTool(), ExplainHandler())
	s.AddTool(updateTool(), UpdateHandler())
	s.AddTool(exploreTool(defaultOntologyRoot), ExploreHandler())
	s.AddTool(retractTool(), RetractHandler())
	s.AddTool(hypothesizeTool(), HypothesizeHandler())
	s.AddTool(reviewTool(), ReviewHandler(s, heartbeat, cache))

	return s
}
