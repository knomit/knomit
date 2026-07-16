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
			Name   string `json:"name"`
			ID     string `json:"id"`
			Branch string `json:"branch"`
			Role   string `json:"role"`
			Source string `json:"source,omitempty"`
		} `json:"mounts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	require.Equal(t, "test", resp.Binding)
	require.Len(t, resp.Mounts, 1)
	require.Equal(t, "test", resp.Mounts[0].Name)
	require.Equal(t, "agent/test", resp.Mounts[0].Branch)
	require.Equal(t, "read+write", resp.Mounts[0].Role)
	require.Len(t, resp.Mounts[0].ID, 40)
}
