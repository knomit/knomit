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
// Review clustering runs in-process over the per-review subgraph via the
// per-repo store.SearchIndex (SubgraphEdges) — no cluster cache or background
// warmer is involved.
func NewServer(profile, defaultOntologyRoot string, readOnly bool, embedders ...store.BatchEmbedder) *server.MCPServer {
	hooks := &server.Hooks{}
	hooks.AddAfterInitialize(func(ctx context.Context, id any, req *mcp.InitializeRequest, result *mcp.InitializeResult) {
		ri, ok := repos.RepoFromContextOpt(ctx)
		if !ok {
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

	for _, t := range enabledTools(toolRegistrations(embedders...), readOnly) {
		s.AddTool(t.tool, t.handler)
	}

	return s
}

// toolReg pairs a tool with its handler and whether it mutates the KB.
type toolReg struct {
	tool    mcp.Tool
	handler server.ToolHandlerFunc
	write   bool
}

// toolRegistrations is the full catalog in registration order.
func toolRegistrations(embedders ...store.BatchEmbedder) []toolReg {
	return []toolReg{
		{learnTool(), LearnHandler(embedders...), true},
		{queryTool(), QueryHandler(), false},
		{explainTool(), ExplainHandler(), false},
		{updateTool(), UpdateHandler(), true},
		{retractTool(), RetractHandler(), true},
		{hypothesizeTool(), HypothesizeHandler(), true},
		{reviewTool(), ReviewHandler(), true},
	}
}

// enabledTools drops write tools when readOnly so a demo instance exposes
// only query + explain.
func enabledTools(regs []toolReg, readOnly bool) []toolReg {
	if !readOnly {
		return regs
	}
	out := regs[:0:0]
	for _, r := range regs {
		if !r.write {
			out = append(out, r)
		}
	}
	return out
}
