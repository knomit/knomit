// Package mcp implements the knomit MCP server.
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/platform/reqinfo"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// NewServer creates the single knomit MCP server instance. Tools are shared
// across all repos and lenses — each handler resolves its binding from the
// request context at call time, and on the unscoped mount the binding gate is
// what puts it there, from the call's own `binding` handle. Instructions are computed per-session in
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
func NewServer(defaultOntologyRoot string, mgr *repos.Manager, readOnly bool, grants auth.Grants, embedders ...store.BatchEmbedder) *server.MCPServer {
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
		recordClientInfo(ctx, mgr, req)
		// The unscoped mount answers initialize with the UNBOUND instructions
		// unconditionally, and since binding became per-CALL that is not a
		// guard against stale state but a plain statement of fact: nothing is
		// bound at initialize because nothing is bound until a tool call
		// carries a handle, and there is no handle yet. A connection to this
		// mount has no repo of its own at any point in its life.
		//
		// The check stays unconditional rather than inspecting the context.
		// Nothing puts a Binding there on this mount today, and a hook that
		// described whatever it found would tell the agent it is bound to repo
		// X while its very next tool call — which is gated on the handle it was
		// never given — refuses. That was a live bug under session-keyed
		// binding; keeping the branch absolute is what stops it returning.
		if repos.SessionScoped(ctx) {
			result.Instructions = ProfileInstructions("code", defaultOntologyRoot, nil) + unboundAddendum
			return
		}
		_, ok := repos.RepoFromContextOpt(ctx)
		if !ok {
			result.Instructions = ProfileInstructions("code", defaultOntologyRoot, nil)
			return
		}
		b := repos.BindingFromContext(ctx) // safe: ri present ⇒ never panics
		result.Instructions = BindingInstructions(b, profileFor(mgr, b.Write()))
	})

	regs := enabledTools(toolRegistrations(mgr, embedders...), readOnly)
	writeTools := make(map[string]bool, len(regs))
	for _, t := range regs {
		if t.write {
			writeTools[t.tool.Name] = true
		}
	}

	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithHooks(hooks),
		// Advertise tasks capability so clients that support it can invoke
		// long-running tools (knomit_review) asynchronously and poll for
		// completion via tasks/get instead of blocking on a single response.
		server.WithTaskCapabilities(true, true, true),
		// tools/list is filtered by what the caller may actually do. This is
		// presentation only — the enforcement is gatePermission below, on the
		// registration seam, because a filter cannot stop a call that never
		// listed.
		server.WithToolFilter(permissionFilter(grants, writeTools)),
	)

	// Both gates wrap the handler BEFORE registration, which is the one seam
	// both dispatch paths share — see gateBinding for why neither a hook nor
	// mcp-go's tool middleware would do.
	//
	// ORDER MATTERS: the permission gate goes on LAST, so it runs FIRST. A
	// caller with no write permission is then told about the permission
	// rather than about a missing handle, and no binding handle is resolved
	// on behalf of a caller who may not write anyway.
	for _, t := range regs {
		h := t.handler
		if t.gate != gateUngated {
			h = gateBinding(mgr, t.tool, t.gate, h)
		}
		if t.write {
			h = gatePermission(grants, t.tool, auth.Write, h)
		}
		s.AddTool(t.tool, h)
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

// toolReg pairs a tool with its handler, whether it mutates the KB, and how the
// binding gate treats its `binding` handle.
type toolReg struct {
	tool    mcp.Tool
	handler server.ToolHandlerFunc
	write   bool
	gate    gateMode
}

// toolRegistrations is the full catalog in registration order.
func toolRegistrations(mgr *repos.Manager, embedders ...store.BatchEmbedder) []toolReg {
	return []toolReg{
		{learnTool(), LearnHandler(embedders...), true, gateRequired},
		{queryTool(), QueryHandler(embedders...), false, gateRequired},
		{explainTool(), ExplainHandler(), false, gateRequired},
		// A pure read of two trees of the upstream: no write path at all.
		{changesTool(), ChangesHandler(), false, gateRequired},
		{updateTool(), UpdateHandler(), true, gateRequired},
		{retractTool(), RetractHandler(), true, gateRequired},
		{hypothesizeTool(), HypothesizeHandler(), true, gateRequired},
		{reviewTool(), ReviewHandler(), true, gateRequired},
		// A WRITE tool, though it authors no fact: open forks a branch,
		// commit merges one into the agent branch and rollback deletes one.
		// A read-only server must not expose it.
		{experimentTool(), ExperimentHandler(mgr), true, gateRequired},
		// Neither is a write tool, and knomit_repos needs no handle: a
		// read-only server still needs both, or nothing on the unscoped mount
		// could be discovered, bound, and therefore read.
		{reposTool(), ReposHandler(mgr), false, gateOptional},
		// knomit_bind is where handles COME FROM, so it is the one tool the
		// gate never wraps. It takes no `binding` argument at all, and
		// rejectUnknownArguments refuses one.
		{bindTool(), BindHandler(mgr), false, gateUngated},
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
