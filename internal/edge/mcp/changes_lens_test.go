package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// changesIn calls knomit_changes under ctx's binding.
func changesIn(t *testing.T, ctx context.Context, args map[string]any) (changesOut, string, bool) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := ChangesHandler()(ctx, req)
	require.NoError(t, err)
	text := resultText(t, res)
	var out changesOut
	if !res.IsError {
		require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	}
	return out, text, res.IsError
}

// On a LENS, knomit_changes reads each member only at the branch the lens pins
// it to — what the lens's REST routes serve — and the lens's write repo also at
// its own agent branch and its experiments (the user's ruling). Any other
// branch of a member, by `branch` or by `consensus`, is refused before it is
// looked up, so the refusal is the same whether that branch exists or not.
// The same repo bound on its own keeps every branch.
//
// Sabotage: drop the `b.FromLens() && !lensServesBranch(...)` refusal in
// ChangesHandler and beta's main (which exists, with a fact on it) is served.
func TestChangesHandler_LensReadsOnlyThePinnedBranches(t *testing.T) {
	m, alpha, beta, _, _, lens := newLensE2E(t)
	svcA, svcB := changesSvc(t, alpha), changesSvc(t, beta)
	writeOn(t, svcA, "agent/test", "kb/tasks/a/a.md")
	writeOn(t, svcB, "agent/test", "kb/tasks/a/b.md")
	writeOn(t, svcB, "main", "kb/tasks/a/b-main.md")
	writeOn(t, svcA, "main", "kb/tasks/a/a-main.md")

	ctx, err := repos.ResolveLensBinding(context.Background(), m, lens)
	require.NoError(t, err)
	b := repos.BindingFromContext(ctx)
	require.True(t, b.FromLens(), "precondition: a binding resolved from a lens")

	// Served: each member's pin, and the write repo's own agent branch.
	out, text, isErr := changesIn(t, ctx, map[string]any{"repo": id12(beta), "prefix": "tasks/a"})
	require.False(t, isErr, text)
	require.Equal(t, "agent/test", out.Branch)
	_, text, isErr = changesIn(t, ctx, map[string]any{"repo": id12(beta), "branch": "agent/test", "prefix": "tasks/a"})
	require.False(t, isErr, "naming the pinned branch explicitly is the pin: %s", text)
	_, text, isErr = changesIn(t, ctx, map[string]any{"branch": "agent/test", "prefix": "tasks/a"})
	require.False(t, isErr, text)

	// The write repo's experiments pass the lens rule; this one does not
	// exist, so the store, not the lens, answers.
	_, text, isErr = changesIn(t, ctx, map[string]any{"branch": "exp/nope", "prefix": "tasks/a"})
	require.True(t, isErr)
	require.Contains(t, text, `has no branch "exp/nope"`, "an experiment of the write repo is not refused by the lens")

	// Refused: every other branch of a member, existing or not, by name or
	// as consensus — read member and write repo alike.
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"read member, existing main", map[string]any{"repo": id12(beta), "branch": "main"}},
		{"read member, missing branch", map[string]any{"repo": id12(beta), "branch": "agent/other"}},
		{"read member, consensus", map[string]any{"repo": id12(beta), "consensus": true}},
		{"read member, an experiment", map[string]any{"repo": id12(beta), "branch": "exp/x"}},
		{"write repo, main", map[string]any{"branch": "main"}},
		{"write repo, consensus", map[string]any{"consensus": true}},
	} {
		_, text, isErr := changesIn(t, ctx, tc.args)
		require.True(t, isErr, "%s: served a branch the lens does not pin: %s", tc.name, text)
		require.True(t, strings.HasPrefix(text, "lens eng reads "), "%s: %s", tc.name, text)
		require.NotContains(t, text, "has no branch", "%s: the refusal must not depend on whether the branch exists", tc.name)
	}

	// The same repo bound on its own is not a lens: every branch stays.
	own, text, isErr := callChanges(t, repos.NewBindingOfRepo(beta, "agent/test"), map[string]any{"branch": "main", "prefix": "tasks/a"})
	require.False(t, isErr, text)
	require.Equal(t, "main", own.Branch)
	require.Len(t, own.Changes, 1)
}
