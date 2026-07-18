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

// TestQuery_MaxResults_RecentCapsAndOrders drives the recency path's cap
// end-to-end. Relevance-mode truncation is covered above; the recency merge caps
// separately via mergeRecent(stamps, maxResults) in queryRecent, which no
// handler-level test exercised. Seed more facts than max_results, query
// sort=recent with a small max_results, page to exhaustion: the union must hold
// exactly max_results rows (never all seeded, never one page's worth) and be
// committed_at-DESC across every page. A regression dropping the mergeRecent cap
// (or the DESC order) fails here.
func TestQuery_MaxResults_RecentCapsAndOrders(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	const seeded = 8
	seedNStoreFacts(t, ctx, seeded)

	const maxResults = 3
	first := runQuery(t, ctx, map[string]any{
		"applies_to": []any{"store"}, "sort": "recent", "max_results": maxResults, "limit": 2,
	})

	var rows []factOutput
	seen := map[string]bool{}
	collect := func(facts []factOutput) {
		for _, f := range facts {
			require.Falsef(t, seen[f.File], "row %s returned twice across pages", f.File)
			seen[f.File] = true
			rows = append(rows, f)
		}
	}
	collect(first.Facts)

	for cursor := first.Cursor; cursor != nil; {
		page := runQuery(t, ctx, map[string]any{"cursor": *cursor, "limit": 2})
		collect(page.Facts)
		cursor = page.Cursor
	}

	require.Len(t, rows, maxResults,
		"sort=recent must cap the total returned rows at max_results, independent of page size")
	for i := 1; i < len(rows); i++ {
		require.GreaterOrEqualf(t, rows[i-1].Frontmatter.CommittedAt, rows[i].Frontmatter.CommittedAt,
			"recent rows must be committed_at-DESC across pages: row %d (%d) precedes row %d (%d)",
			i-1, rows[i-1].Frontmatter.CommittedAt, i, rows[i].Frontmatter.CommittedAt)
	}
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
