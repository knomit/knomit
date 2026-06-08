package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// seedManyPrinciples writes n distinct policy-typed principle facts in one learn
// moment. Bodies are given distinct lengths (bodyPrefix + i 'x's) so the
// length-based mock embedder produces distinct vectors and dedup does not
// collapse them. Returns nothing; tests recover paths from query results.
func seedManyPrinciples(t *testing.T, ctx context.Context, n int, bodyPrefix string) {
	t.Helper()
	facts := make([]any, n)
	for i := range n {
		facts[i] = map[string]any{
			"topic":      "principles",
			"category":   "mission/store",
			"title":      fmt.Sprintf("Principle %04d", i),
			"body":       bodyPrefix + strings.Repeat("x", i),
			"kind":       "pragmatic",
			"type":       "policy",
			"domain":     []any{"store"},
			"confidence": 0.8,
			"sources":    1,
			"entities":   []any{"designer"},
			"refs":       []any{},
		}
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": "seed-many", "facts": facts}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "seed failed: %s", resultText(t, result))
}

// runQuery is a small helper that calls QueryHandler with the given args and
// decodes the envelope.
func runQuery(t *testing.T, ctx context.Context, args map[string]any) queryResponse {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "query failed: %s", resultText(t, result))
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &resp))
	return resp
}

// TestQuery_PagesThroughAllResults pins cursor pagination: a result set larger
// than one page returns a first page + cursor, and walking the cursor returns
// every fact exactly once with score carried onto resumed pages (the snapshot
// fix).
func TestQuery_PagesThroughAllResults(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	const n = 25 // > defaultPageSize (20)
	seedManyPrinciples(t, ctx, n, "policy body ")

	// Filter-only query returns all policy facts (deterministic, score 100).
	first := runQuery(t, ctx, map[string]any{"type": []any{"policy"}})
	require.Len(t, first.Facts, defaultPageSize, "first page must be a full page")
	require.True(t, first.HasMore, "more results remain")
	require.NotNil(t, first.Cursor, "cursor must be returned while more remain")

	seen := map[string]bool{}
	collect := func(facts []factOutput) {
		for _, f := range facts {
			require.False(t, seen[f.File], "fact %s returned twice across pages", f.File)
			seen[f.File] = true
			require.Greater(t, f.Score, 0.0, "score must be present on every page (incl. resumed)")
		}
	}
	collect(first.Facts)

	second := runQuery(t, ctx, map[string]any{"cursor": *first.Cursor})
	require.Len(t, second.Facts, n-defaultPageSize, "second page holds the remainder")
	require.False(t, second.HasMore, "no more results after the last page")
	require.Nil(t, second.Cursor, "cursor must be nil once drained")
	collect(second.Facts)

	require.Len(t, seen, n, "every seeded fact must appear exactly once across pages")
}

// TestQuery_SnippetByDefault_FullWithIncludeBody pins snippet-by-default and the
// include_body escape hatch on a single fact with a long body.
func TestQuery_SnippetByDefault_FullWithIncludeBody(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	longBody := strings.Repeat("lorem ipsum dolor ", 200) // ~3600 chars, >> snippetMaxRunes
	seedManyPrinciples(t, ctx, 1, longBody)

	// Default: snippet, truncated.
	snip := runQuery(t, ctx, map[string]any{"type": []any{"policy"}})
	require.Len(t, snip.Facts, 1)
	f := snip.Facts[0]
	require.True(t, f.BodyTruncated, "long body must be marked truncated by default")
	require.LessOrEqual(t, len([]rune(f.Body)), snippetMaxRunes+1, "snippet must be bounded (+1 for ellipsis)")
	require.Less(t, len(f.Body), len(longBody), "snippet must be shorter than the full body")

	// include_body: full body, not truncated.
	full := runQuery(t, ctx, map[string]any{"type": []any{"policy"}, "include_body": true})
	require.Len(t, full.Facts, 1)
	require.False(t, full.Facts[0].BodyTruncated, "include_body must not mark truncation")
	require.Contains(t, full.Facts[0].Body, "lorem ipsum dolor", "include_body must return the full body")
	require.Greater(t, len([]rune(full.Facts[0].Body)), snippetMaxRunes*5, "full body must be far larger than a snippet")
}

// TestQuery_IncludeBodyAcrossPages pins that include_body works on resumed pages
// too (version-pinned re-read), and that include_body caps the page size.
func TestQuery_IncludeBodyAcrossPages(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	const n = 12
	seedManyPrinciples(t, ctx, n, "a reasonably sized policy body that exceeds the snippet limit "+strings.Repeat("y", 400))

	first := runQuery(t, ctx, map[string]any{"type": []any{"policy"}, "include_body": true})
	require.Len(t, first.Facts, includeBodyDefaultPage, "include_body default page size")
	require.True(t, first.HasMore)
	require.NotNil(t, first.Cursor)
	for _, f := range first.Facts {
		require.False(t, f.BodyTruncated, "include_body page-1 rows must carry full bodies")
	}

	// Resume with include_body: full bodies re-read from the frozen commit.
	second := runQuery(t, ctx, map[string]any{"cursor": *first.Cursor, "include_body": true})
	require.NotEmpty(t, second.Facts)
	for _, f := range second.Facts {
		require.False(t, f.BodyTruncated, "include_body resumed rows must be full, not snippets")
		require.Greater(t, len([]rune(f.Body)), snippetMaxRunes, "resumed full body must exceed the snippet bound")
	}
}

// TestQuery_ExpiredCursor pins the guidance error for an unknown/expired cursor.
func TestQuery_ExpiredCursor(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"cursor": "does-not-exist"}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "expired cursor must surface as an error")
	require.Contains(t, resultText(t, result), "session expired or not found")
}

// TestQuery_LargeResultSetDoesNotFlood is the regression for the original bug:
// querying a corpus of large facts must NOT return one oversized blob. The first
// page is bounded in count and each body is a small snippet, with a cursor for
// the rest.
func TestQuery_LargeResultSetDoesNotFlood(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	const n = 25
	// Each body ~3KB; pre-fix flood was ~25 * 3KB in one response.
	seedManyPrinciples(t, ctx, n, strings.Repeat("synthesis-ish body text ", 130))

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"type": []any{"policy"}}
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := resultText(t, result)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Facts, defaultPageSize, "first page must be bounded in count")
	require.True(t, resp.HasMore)
	require.NotNil(t, resp.Cursor)
	for _, f := range resp.Facts {
		require.True(t, f.BodyTruncated, "each row must be a snippet, not a full body")
		require.LessOrEqual(t, len([]rune(f.Body)), snippetMaxRunes+1)
	}
	// Whole-response sanity bound: comfortably under the tool-result size cap
	// that the pre-fix 69K blob exceeded.
	require.Less(t, len(text), 40_000, "snippet page must stay well under the size cap")
}

// TestQuery_SmallResultSetNoCursor pins the fast path: a result set that fits in
// one page returns no cursor and creates no session.
func TestQuery_SmallResultSetNoCursor(t *testing.T) {
	_, ctx, _ := newPrinciplesTestRepo(t)
	seedManyPrinciples(t, ctx, 3, "small body ")

	resp := runQuery(t, ctx, map[string]any{"type": []any{"policy"}})
	require.Len(t, resp.Facts, 3)
	require.False(t, resp.HasMore)
	require.Nil(t, resp.Cursor, "single-page result must not return a cursor")
}
