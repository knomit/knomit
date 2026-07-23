package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
)

func TestReposHandler_ListsMounts(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "agent/test"))

	var req mcpgo.CallToolRequest
	result, err := ReposHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	var resp struct {
		Binding string `json:"binding"`
		Mounts  []struct {
			Name        string `json:"name"`
			ID          string `json:"id"`
			Branch      string `json:"branch"`
			Role        string `json:"role"`
			Source      string `json:"source,omitempty"`
			WriteBranch string `json:"write_branch,omitempty"`
		} `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.Equal(t, "test", resp.Binding)
	require.Len(t, resp.Mounts, 1)
	require.Equal(t, "test", resp.Mounts[0].Name)
	require.Equal(t, "agent/test", resp.Mounts[0].Branch)
	require.Equal(t, "read+write", resp.Mounts[0].Role)
	require.Equal(t, "agent/test", resp.Mounts[0].WriteBranch,
		"the read+write row surfaces the write target (agent branch)")
	// The mount id is the 12-hex wire form (federate.ID12), matching kb://<id>/… paths
	// and the AfterInitialize instructions mount table — not the full hash (M-3).
	require.Len(t, resp.Mounts[0].ID, 12)
	require.Equal(t, federate.ID12(ri.ID()), resp.Mounts[0].ID)
}

// TestReposHandler_ReadOnlyView pins the discovery contract for a read-only
// view: a lens-of-one bound to a non-agent branch (here "main") is not
// writable, so its single mount must advertise role "read" — never
// "read+write". Regression guard for the WriteOK() omission in the role rule.
func TestReposHandler_ReadOnlyView(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "main"))

	var req mcpgo.CallToolRequest
	result, err := ReposHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	var resp struct {
		Binding string `json:"binding"`
		Mounts  []struct {
			Branch      string `json:"branch"`
			Role        string `json:"role"`
			WriteBranch string `json:"write_branch,omitempty"`
		} `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.Len(t, resp.Mounts, 1)
	require.Equal(t, "main", resp.Mounts[0].Branch)
	require.Equal(t, "read", resp.Mounts[0].Role)
	require.Empty(t, resp.Mounts[0].WriteBranch, "a read-only view has no write target")
}

// TestReposHandler_WriteBranchSurfacesAgentTarget covers the misleading case
// M-4 addresses: when a lens pins the write repo's READ mount to a non-agent
// branch, the branch column shows that pinned read branch, but writes still
// land on the write repo's agent branch (RFC decision 19). The read+write row
// must surface both — write_branch = agent branch, branch = the pinned read
// branch — while a foreign read mount carries no write_branch at all.
func TestReposHandler_WriteBranchSurfacesAgentTarget(t *testing.T) {
	writeRepo := newLearnTestRepo(t, fact.CodeOntology())
	readRepo := newLearnTestRepo(t, fact.CodeOntology())
	// Write repo's own read mount is pinned to "main", NOT its agent branch.
	b := repos.NewBindingForTest(writeRepo,
		repos.ReadTarget{RI: writeRepo, Branch: "main"},
		repos.ReadTarget{RI: readRepo, Branch: "agent/test", Source: "core-src"},
	)
	ctx := repos.WithBinding(context.Background(), b)

	var req mcpgo.CallToolRequest
	result, err := ReposHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))

	type mountRow struct {
		ID          string `json:"id"`
		Branch      string `json:"branch"`
		Role        string `json:"role"`
		WriteBranch string `json:"write_branch,omitempty"`
	}
	var resp struct {
		Mounts []mountRow `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.Len(t, resp.Mounts, 2)

	byID := map[string]mountRow{}
	for _, m := range resp.Mounts {
		byID[m.ID] = m
	}

	w := byID[federate.ID12(writeRepo.ID())]
	require.Equal(t, "read+write", w.Role)
	require.Equal(t, "main", w.Branch, "branch column shows the pinned READ branch")
	require.Equal(t, writeRepo.AgentBranch(), w.WriteBranch,
		"write_branch shows the agent branch writes actually target, not the read branch")
	require.NotEqual(t, w.Branch, w.WriteBranch, "the misleading case: read branch != write target")

	r := byID[federate.ID12(readRepo.ID())]
	require.Equal(t, "read", r.Role)
	require.Empty(t, r.WriteBranch, "read mounts carry no write_branch")
}
