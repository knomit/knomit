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

// TestQuery_MaxResults_RecentLensOfOnePagesAndOrders exercises the recency
// handler end-to-end for a lens-of-one: sort=recent + max_results + cursor
// paging, asserting committed_at exposure and DESC ordering across pages. NOTE
// this does NOT pin mergeRecent's cross-mount cap — with a single mount the
// per-mount SQL LIMIT (q.Limit = maxResults, search_query.go) already bounds the
// snapshot to max_results, so mergeRecent trims nothing. The mergeRecent cap is
// pinned separately by TestQuery_MaxResults_RecentCapsAcrossMounts below, where
// each mount stays under max_results but the union exceeds it.
func TestQuery_MaxResults_RecentLensOfOnePagesAndOrders(t *testing.T) {
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

	// Single-mount depth cap (via the per-mount SQL LIMIT) bounds the snapshot.
	require.Len(t, rows, maxResults,
		"sort=recent snapshot depth must be bounded to max_results, independent of page size")
	for i := 1; i < len(rows); i++ {
		require.GreaterOrEqualf(t, rows[i-1].Frontmatter.CommittedAt, rows[i].Frontmatter.CommittedAt,
			"recent rows must be committed_at-DESC across pages: row %d (%d) precedes row %d (%d)",
			i-1, rows[i-1].Frontmatter.CommittedAt, i, rows[i].Frontmatter.CommittedAt)
	}
}

// TestQuery_MaxResults_RecentCapsAcrossMounts pins mergeRecent's cross-mount cap
// (federate.go: `if len(out) > max { out = out[:max] }`), which no handler-level
// test exercised. The trap the reviewer found: with a single mount the per-mount
// SQL LIMIT already bounds the snapshot, so mergeRecent's cap is dead weight and
// removing it changes nothing. This test defeats that by using a genuine
// two-mount lens where each mount holds FEWER facts than max_results (so the
// per-mount SQL LIMIT never trims) but the UNION exceeds it — only the
// cross-mount cap can bound the result. Seed 2 mounts x 2 recency facts,
// sort=recent (no text → the mergeRecent path, not RRF), max_results=3, page to
// exhaustion: the fused union must hold exactly 3 rows, committed_at-DESC. A
// regression dropping the mergeRecent cap returns all 4 and fails here.
func TestQuery_MaxResults_RecentCapsAcrossMounts(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	const perMount = 2 // < maxResults, so the per-mount SQL LIMIT never trims a mount
	seedFedMany(t, ctxA, perMount, "Alpha", "alpha body ", "store")
	seedFedMany(t, ctxB, perMount, "Bravo", "bravo body ", "ui")

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	ctx := repos.WithBinding(context.Background(), b)

	const maxResults = 3 // < 2*perMount (4): only the cross-mount cap can bound the union
	first := runQuery(t, ctx, map[string]any{
		"type": []any{"policy"}, "sort": "recent", "max_results": maxResults, "limit": 2,
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
		"sort=recent over a multi-mount lens must cap the fused union at max_results via mergeRecent")
	for i := 1; i < len(rows); i++ {
		require.GreaterOrEqualf(t, rows[i-1].Frontmatter.CommittedAt, rows[i].Frontmatter.CommittedAt,
			"fused recent rows must be committed_at-DESC across pages: row %d (%d) precedes row %d (%d)",
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
