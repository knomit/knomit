package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"knomit/internal/auth"
)

// permissionFilter hides tools the caller may not invoke from tools/list. It
// is a COURTESY, not the check: a read-only client is spared being offered
// knomit_learn, but nothing stops a client that never listed from calling it.
// gatePermission is what refuses.
//
// nil grants means no filtering, for in-process callers and tests; the HTTP
// edge always passes a non-nil store.
func permissionFilter(grants auth.Grants, writeTools map[string]bool) mcpserver.ToolFilterFunc {
	return func(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
		if grants == nil {
			return tools
		}
		p, _ := auth.FromContext(ctx)
		if auth.Allowed(ctx, grants, p, auth.Write) {
			return tools
		}
		out := tools[:0:0] // a fresh array: never alias the caller's slice
		for _, t := range tools {
			if !writeTools[t.Name] {
				out = append(out, t)
			}
		}
		return out
	}
}

// gatePermission is the call-time twin of permissionFilter, and the one that
// actually enforces.
//
// It wraps the handler BEFORE registration, the same seam gateBinding uses,
// and for the same reason: mcp-go's tool-middleware chain is NOT applied on
// the task-augmented dispatch path, so a tool declaring TaskSupport can be
// invoked with params.task set and reach the registered handler directly.
// knomit_review and knomit_hypothesize both declare it and both are write
// tools, so a middleware gate would leave exactly those two reachable
// ungated. See kb/invariants/mcp/binding-gate/registration-seam.
//
// nil grants is a no-op, matching permissionFilter.
func gatePermission(grants auth.Grants, tool mcpgo.Tool, perm auth.Permission, next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	if grants == nil {
		return next
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		p, _ := auth.FromContext(ctx)
		if !auth.Allowed(ctx, grants, p, perm) {
			// A tool ERROR, not a transport error: the caller is an agent, and
			// an agent that is told which permission it lacks and under which
			// principal can ask for exactly that instead of retrying.
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"permission denied: %s requires %s; principal %s does not hold it",
				tool.Name, perm, p.String())), nil
		}
		return next(ctx, req)
	}
}
