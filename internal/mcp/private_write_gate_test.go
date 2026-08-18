package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// agentCtx is the writable-branch context every test below uses. Same shape as
// TestWriteHandlers_AgentBranchStaysWritable in write_gate_test.go.
func agentCtx(t *testing.T) context.Context {
	t.Helper()
	ri := newLearnTestRepo(t, fact.CodeOntology())
	return repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
}

func callTool(t *testing.T, h func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error),
	ctx context.Context, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := h(ctx, req)
	require.NoError(t, err)
	return result
}

// knomit_update refuses every private path EXCEPT knomit's own namespace.
func TestUpdate_NonWritablePrivatePathRefused(t *testing.T) {
	ctx := agentCtx(t)
	for _, p := range []string{"kb/.drafts/x.md", ".github/x.md", ".knomit/x.md"} {
		result := callTool(t, UpdateHandler(), ctx, map[string]any{
			"file":        p,
			"moment_name": "m",
			"body":        "b",
		})
		require.Truef(t, result.IsError, "update must refuse %s", p)
		require.Containsf(t, resultText(t, result), ".knomit/<area>/",
			"error for %s must name the writable shape", p)
	}
}

// A DELETE is a write: retract obeys the same rule. Before this change retract
// had no private check at all.
func TestRetract_NonWritablePrivatePathRefused(t *testing.T) {
	ctx := agentCtx(t)
	result := callTool(t, RetractHandler(), ctx, map[string]any{
		"file":        "kb/.drafts/x.md",
		"moment_name": "m",
		"reason":      "r",
	})
	require.True(t, result.IsError, "retract must refuse a non-writable private path")
	require.Contains(t, resultText(t, result), ".knomit/<area>/")
}
