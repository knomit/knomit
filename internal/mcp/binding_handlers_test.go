package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// A handler must resolve its repo through the Binding: an explicit binding
// with NO bare RepoInstance in context is sufficient.
func TestHandlers_ResolveRepoViaBinding(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	b := repos.NewBindingOfRepo(ri, "agent/test")
	ctx := repos.WithBinding(context.Background(), b)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"applies_to": []any{"store"}}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, resultText(t, result))
}
