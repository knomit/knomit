package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

func TestQueryHandler_HonorsBoundBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	base := repos.WithRepoInstance(context.Background(), ri)

	seedPrincipleWithDomain(t, base, "seed-branch", "mission/store", "Branch Honoring Fact", "store")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"applies_to": []any{"store"}}

	// Bound to the agent branch: the fact is visible.
	onAgent, err := QueryHandler()(repos.WithBranch(base, "agent/test"), req)
	require.NoError(t, err)
	require.False(t, onAgent.IsError, resultText(t, onAgent))
	require.Contains(t, resultText(t, onAgent), "Branch Honoring Fact")

	// Bound to main (exists, but sits at the init commit with no facts):
	// the fact must NOT appear. Before the fix the handler ignored the
	// bound branch and used AgentBranch(), so this assertion fails on HEAD.
	onMain, err := QueryHandler()(repos.WithBranch(base, "main"), req)
	require.NoError(t, err)
	require.False(t, onMain.IsError, resultText(t, onMain))
	require.NotContains(t, resultText(t, onMain), "Branch Honoring Fact")
}

func TestExplainHandler_HonorsBoundBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	base := repos.WithRepoInstance(context.Background(), ri)

	path := seedPrincipleWithDomain(t, base, "seed-explain", "mission/store", "Explain Branch Fact", "store")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": path}

	// Bound to the agent branch: explain succeeds.
	onAgent, err := ExplainHandler()(repos.WithBranch(base, "agent/test"), req)
	require.NoError(t, err)
	require.False(t, onAgent.IsError, resultText(t, onAgent))

	// Bound to main: the fact does not exist on that branch.
	onMain, err := ExplainHandler()(repos.WithBranch(base, "main"), req)
	require.NoError(t, err)
	require.True(t, onMain.IsError, "explain on a branch without the fact must error; got: %s", resultText(t, onMain))
}
