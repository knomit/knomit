package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/foundation/auth"
)

// asCaller puts an endpoint and a principal on ctx, as the web layer's
// HTTPContextFunc and the auth edge do for every MCP request.
func asCaller(ctx context.Context, endpoint, subject string) context.Context {
	return WithEndpoint(auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindHost, ID: subject, Via: auth.ViaToken}), endpoint)
}

const (
	ownerEndpointA = "/api/v1/repos/test/branches/agent:test/mcp"
	ownerEndpointB = "/api/v1/mcp"
)

// A tool-session cursor (tool_sessions + tool_queue) belongs to the endpoint
// AND the principal that minted it (audit G2). The same binding resumed by
// another principal on the same endpoint, or by the same principal through
// another endpoint, reads as expired; the owner itself gets page 2.
//
// Sabotage: drop the `sess.Owner != callerOwner(ctx)` check in queryResume and
// both foreign resumes serve page 2.
func TestQueryResume_CursorIsOwnedByEndpointAndPrincipal(t *testing.T) {
	_, base, _ := newPrinciplesTestRepo(t)
	seedManyPrinciples(t, base, 25, "policy body ")
	owner := asCaller(base, ownerEndpointA, "alice")
	first := runQuery(t, owner, map[string]any{"type": []any{"policy"}})
	require.NotNil(t, first.Cursor)

	for name, ctx := range map[string]context.Context{
		"another principal, same endpoint": asCaller(base, ownerEndpointA, "bob"),
		"same principal, another endpoint": asCaller(base, ownerEndpointB, "alice"),
	} {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{"cursor": *first.Cursor}
		res, err := QueryHandler()(ctx, req)
		require.NoError(t, err)
		require.True(t, res.IsError, "%s: resumed another owner's cursor: %s", name, resultText(t, res))
		require.Equal(t, "session expired or not found — omit cursor to start a new query", resultText(t, res), name)
	}

	// The refusals consumed nothing: the owner reads the rest.
	second := runQuery(t, owner, map[string]any{"cursor": *first.Cursor})
	require.Len(t, second.Facts, 5, "the owner's page 2 is intact")
}

// The explain cursor follows the same rule.
//
// Sabotage: drop the `session.Owner != callerOwner(ctx)` check in
// explainResume.
func TestExplainResume_CursorIsOwnedByEndpointAndPrincipal(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	base := repos.WithRepoInstance(context.Background(), ri)
	writeExplainFact(t, base, ri, "kb/child.md", "Child", 0.88, nil)
	writeExplainFact(t, base, ri, "kb/root.md", "Root", 0.90, []string{"kb/child.md"})

	owner := asCaller(base, ownerEndpointA, "alice")
	var first mcpgo.CallToolRequest
	first.Params.Arguments = map[string]any{"file": "kb/root.md"}
	res, err := ExplainHandler()(owner, first)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	var resp expResp
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &resp))
	require.NotNil(t, resp.Cursor)

	resume := func(ctx context.Context) *mcpgo.CallToolResult {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{"file": "kb/root.md", "cursor": *resp.Cursor}
		out, err := ExplainHandler()(ctx, req)
		require.NoError(t, err)
		return out
	}
	for name, ctx := range map[string]context.Context{
		"another principal, same endpoint": asCaller(base, ownerEndpointA, "bob"),
		"same principal, another endpoint": asCaller(base, ownerEndpointB, "alice"),
	} {
		out := resume(ctx)
		require.True(t, out.IsError, "%s: resumed another owner's cursor: %s", name, resultText(t, out))
		require.Equal(t, "session expired or not found — omit cursor to start a new session", resultText(t, out), name)
	}
	out := resume(owner)
	require.False(t, out.IsError, resultText(t, out))
	require.Contains(t, resultText(t, out), "Child", "the owner continues the walk")
}

// And the materialize cursor.
//
// Sabotage: drop `session.Owner != callerOwner(ctx)` in materializeResume.
func TestMaterializeResume_CursorIsOwnedByEndpointAndPrincipal(t *testing.T) {
	m, alpha, _, _, _, _ := newLensE2E(t)
	base := auth.WithRepoReach(repos.WithRepoInstance(context.Background(), alpha), auth.AllRepos)
	obs(t, alpha, "kb/a/y.md", "Y")
	obs(t, alpha, "kb/a/r.md", "R", "kb/a/y.md")

	owner := asCaller(base, ownerEndpointA, "alice")
	p1 := matCall(t, owner, m, map[string]any{"file": "kb/a/r.md", "audience": "human", "limit": 1})
	require.True(t, p1.HasMore)

	for name, ctx := range map[string]context.Context{
		"another principal, same endpoint": asCaller(base, ownerEndpointA, "bob"),
		"same principal, another endpoint": asCaller(base, ownerEndpointB, "alice"),
	} {
		res, text := matCallRaw(t, ctx, m, map[string]any{"cursor": *p1.Cursor, "limit": 1})
		require.True(t, res.IsError, "%s: resumed another owner's cursor: %s", name, text)
		require.True(t, strings.HasPrefix(text, errMaterializeExpired), "%s: %s", name, text)
	}
	p2 := matCall(t, owner, m, map[string]any{"cursor": *p1.Cursor, "limit": 1})
	require.Equal(t, []string{"Y"}, titles(p2.Facts))
}

// callerOwner's two halves are both in the key, and the principal is the
// identity string (kind:id@via), so two transports of one subject differ only
// when the identity does.
func TestCallerOwner_EndpointAndPrincipal(t *testing.T) {
	base := context.Background()
	require.Equal(t, "", callerOwner(base), "no endpoint and no principal: the in-process owner")
	a := callerOwner(asCaller(base, ownerEndpointA, "alice"))
	require.Equal(t, ownerEndpointA+" host:alice@token", a)
	require.Equal(t, a, callerOwner(asCaller(base, ownerEndpointA+"/", "alice")), "a trailing / is the same endpoint")
	require.NotEqual(t, a, callerOwner(asCaller(base, ownerEndpointA, "bob")))
	require.NotEqual(t, a, callerOwner(asCaller(base, ownerEndpointB, "alice")))
}
