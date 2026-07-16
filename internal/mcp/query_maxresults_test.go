package mcp

import (
	"context"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

func seedNStoreFacts(t *testing.T, ctx context.Context, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedPrincipleWithDomain(t, ctx, fmt.Sprintf("seed-%d", i), "mission/store",
			fmt.Sprintf("Depth Fact %d", i), "store")
	}
}

// runQuery is shared with query_paging_test.go in this package.

// max_results caps the total snapshot depth, independent of page size.
func TestQuery_MaxResults_CapsSnapshotDepth(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedNStoreFacts(t, ctx, 5)

	// Depth 2 with a page size larger than the depth: exactly 2 facts, no cursor.
	resp := runQuery(t, ctx, map[string]any{"applies_to": []any{"store"}, "max_results": 2, "limit": 10})
	require.Len(t, resp.Facts, 2)
	require.Nil(t, resp.Cursor)
	require.False(t, resp.HasMore)

	// Depth 3 with page size 2: first page 2, cursor pages the third.
	resp = runQuery(t, ctx, map[string]any{"applies_to": []any{"store"}, "max_results": 3, "limit": 2})
	require.Len(t, resp.Facts, 2)
	require.NotNil(t, resp.Cursor)
	page2 := runQuery(t, ctx, map[string]any{"cursor": *resp.Cursor, "limit": 2})
	require.Len(t, page2.Facts, 1)

	// Default (absent): all 5 facts reachable.
	resp = runQuery(t, ctx, map[string]any{"applies_to": []any{"store"}, "limit": 10})
	require.Len(t, resp.Facts, 5)

	// Values above the ceiling clamp (behave like the default).
	resp = runQuery(t, ctx, map[string]any{"applies_to": []any{"store"}, "max_results": 100000, "limit": 10})
	require.Len(t, resp.Facts, 5)
}

func TestQuery_MaxResults_RejectsNonPositive(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"applies_to": []any{"store"}, "max_results": -1}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, resultText(t, result), "max_results")
}
