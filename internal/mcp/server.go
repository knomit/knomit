// Package mcp implements the knomit MCP server.
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/obs/reqinfo"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// NewServer creates the single knomit MCP server instance. Tools are shared
// across all repos and lenses — each handler resolves its binding from the
// request context at call time. Instructions are computed per-session in
// AfterInitialize: the authoring addendum comes from the context repo's
// per-repo profile (the registry row's profile column — lenses RFC decision
// 12), so one instance replaces the three formerly profile-keyed ones.
//
// mgr may be nil (tests, degraded callers) — the profile then resolves to
// "code".
//
// Review clustering runs in-process over the per-review subgraph via the
// per-repo store.GraphStore (SubgraphEdges) — no cluster cache or background
// warmer is involved.
func NewServer(defaultOntologyRoot string, mgr *repos.Manager, readOnly bool, embedders ...store.BatchEmbedder) *server.MCPServer {
	hooks := &server.Hooks{}
	// Name the tool for the HTTP layer. Over streamable HTTP every call is the
	// same POST .../mcp, so without this a slow-request warning cannot say which
	// tool was slow. The annotation rides in the request context (mcp-go derives
	// tool-call contexts from the HTTP request's); FromContext is nil-safe, so
	// stdio sessions — which have no HTTP request — simply record nothing.
	hooks.AddBeforeCallTool(func(ctx context.Context, _ any, req *mcp.CallToolRequest) {
		reqinfo.FromContext(ctx).SetTool(req.Params.Name)
	})
	hooks.AddAfterInitialize(func(ctx context.Context, id any, req *mcp.InitializeRequest, result *mcp.InitializeResult) {
		_, ok := repos.RepoFromContextOpt(ctx)
		if !ok {
			result.Instructions = ProfileInstructions("code", defaultOntologyRoot, nil)
			return
		}
		b := repos.BindingFromContext(ctx) // safe: ri present ⇒ never panics
		result.Instructions = BindingInstructions(b, profileFor(mgr, b.Write()))
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

// profileFor resolves the per-repo authoring profile, defaulting to
// ProfileCode whenever the manager, registry, repo identity, or stored row is
// unavailable — the default must never error a session.
//
// Profile is keyed by the registry uid, not by the root commit: a repo that
// adopts a remote's divergent history keeps its serving profile.
func profileFor(mgr *repos.Manager, ri *repos.RepoInstance) string {
	p := repos.ProfileCode
	if mgr == nil || ri == nil {
		return p
	}
	reg := mgr.Repos()
	if reg == nil {
		return p
	}
	rec, ok, err := reg.Get(ri.UID())
	if err != nil {
		log.Warn().Err(err).Str("repo", ri.Name()).Msg("profile lookup failed; serving the default")
		return p
	}
	if ok && rec.Profile != "" {
		p = rec.Profile
	}
	return p
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
		{reposTool(), ReposHandler(), false},
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
