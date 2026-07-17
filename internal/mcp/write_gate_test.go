package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// All five write tools must refuse to operate when the binding's branch is
// not writable (v1: anything other than the repo's own agent branch).
func TestWriteHandlers_RejectNonWritableBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onMain := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "main")

	handlers := map[string]func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error){
		"learn":       LearnHandler(),
		"update":      UpdateHandler(),
		"retract":     RetractHandler(),
		"hypothesize": HypothesizeHandler(),
		"review":      ReviewHandler(),
	}

	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{}
			result, err := h(onMain, req)
			require.NoError(t, err)
			require.True(t, result.IsError, "%s must reject writes on main", name)
			require.Contains(t, resultText(t, result), "read-only view", name)
		})
	}
}

// A lens-shaped read-only view (an explicit binding pinning the write repo's
// read mount to a non-agent branch) rejects writes and names that branch —
// the gate reads Binding.WriteOK()/WriteMountBranch(), not the context branch.
func TestWriteHandlers_LensReadOnlyViewNamesMountBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	b := repos.NewBindingOfRepo(ri, "main") // main != agent branch → read-only view
	ctx := repos.WithBinding(context.Background(), b)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "read-only view must reject learn")
	text := resultText(t, result)
	require.Contains(t, text, "read-only view")
	require.Contains(t, text, `branch "main"`, "error names the write repo's read-mount branch")
}

// The agent branch stays writable — learn on the bound agent branch succeeds.
func TestWriteHandlers_AgentBranchStaysWritable(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
	seedPrincipleWithDomain(t, onAgent, "seed-writable", "mission/store", "Writable Fact", "store")
}

// The other four write handlers must also PASS the writable-branch gate when
// bound to the agent branch. We do not set up full happy paths: minimal args
// may trip some OTHER validation error (e.g. "file is required"), which is
// fine — we assert only that the gate error ("read-only view") is absent, so
// gate-pass behavior is pinned for every write tool, not just learn.
func TestWriteHandlers_AgentBranchPassesGate(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	handlers := map[string]func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error){
		"update":      UpdateHandler(),
		"retract":     RetractHandler(),
		"hypothesize": HypothesizeHandler(),
		"review":      ReviewHandler(),
	}

	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{}
			result, err := h(onAgent, req)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotContains(t, resultText(t, result), "read-only view",
				"%s must pass the writable-branch gate on the agent branch", name)
		})
	}
}
