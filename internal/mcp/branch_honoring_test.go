package mcp

import (
	"context"
	"encoding/json"
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

// A query cursor minted while bound to one branch must not be resumable while
// bound to another: it is a frozen view of a single binding (lenses RFC §7.3).
// Resuming cross-branch must be rejected before any dequeue side effect, not
// silently rehydrated against the wrong branch's state.
func TestQueryResume_RejectsCrossBranchCursor(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	base := repos.WithRepoInstance(context.Background(), ri)
	onAgent := repos.WithBranch(base, "agent/test")

	// Seed >1 page of facts on the agent branch; limit=2 leaves a remainder,
	// so the first page comes back with a cursor.
	seedNStoreFacts(t, onAgent, 3)
	first := runQuery(t, onAgent, map[string]any{"applies_to": []any{"store"}, "limit": 2})
	require.NotNil(t, first.Cursor, "a cursor must be returned while results remain")

	// Resume bound to main: cross-branch, must be rejected (build the request by
	// hand since runQuery asserts !IsError).
	onMain := repos.WithBranch(base, "main")
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"cursor": *first.Cursor, "limit": 2}
	result, err := QueryHandler()(onMain, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "resuming a cross-branch cursor must error")
	require.Contains(t, resultText(t, result), "cursor was created on branch")

	// Happy path: resuming bound to the creation branch still works, and the
	// gate must not have consumed the queue (the remainder is intact).
	second := runQuery(t, onAgent, map[string]any{"cursor": *first.Cursor, "limit": 2})
	require.Len(t, second.Facts, 1, "resuming on the creation branch returns the remainder")
}

// Same frozen-view rule for explain cursors: a root fact referencing a child
// enqueues that child on the first call, so a cursor comes back cheaply. That
// cursor must not resume across a branch boundary.
func TestExplainResume_RejectsCrossBranchCursor(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	base := repos.WithRepoInstance(context.Background(), ri)
	onAgent := repos.WithBranch(base, "agent/test")

	childPath := "kb/observations/testing/child.md"
	writeExplainFact(t, onAgent, ri, childPath, "Child", 0.9, nil)
	rootPath := "kb/observations/testing/root.md"
	writeExplainFact(t, onAgent, ri, rootPath, "Root", 0.9, []string{childPath})

	// First explain call bound to agent/test enqueues the child → cursor.
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": rootPath}
	result, err := ExplainHandler()(onAgent, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))
	var resp expResp
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.NotNil(t, resp.Cursor, "explain must return a cursor with a child queued")

	// Resume bound to main: cross-branch, must be rejected.
	onMain := repos.WithBranch(base, "main")
	var mreq mcpgo.CallToolRequest
	mreq.Params.Arguments = map[string]any{"file": rootPath, "cursor": *resp.Cursor}
	mResult, err := ExplainHandler()(onMain, mreq)
	require.NoError(t, err)
	require.True(t, mResult.IsError, "cross-branch explain resume must error")
	require.Contains(t, resultText(t, mResult), "cursor was created on branch")

	// Happy path: resuming bound to the creation branch still works (the gate
	// ran before any dequeue side effect, so the queue is intact).
	var hreq mcpgo.CallToolRequest
	hreq.Params.Arguments = map[string]any{"file": rootPath, "cursor": *resp.Cursor}
	hResult, err := ExplainHandler()(onAgent, hreq)
	require.NoError(t, err)
	require.False(t, hResult.IsError, resultText(t, hResult))
}
