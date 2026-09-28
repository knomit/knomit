package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

type expHistoryPage struct {
	Path    string          `json:"path"`
	History *expHistory     `json:"history"`
	Facts   json.RawMessage `json:"facts"`
}

func callExplain(t *testing.T, ctx context.Context, args map[string]any) (string, bool) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := ExplainHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return resultText(t, result), result.IsError
}

// mergeDeliveredHistory builds the agent-branch-by-PR topology: every write
// to path lands on the bound agent branch, reaches main through a real
// two-parent merge, and the agent branch then catches up with main. Returns
// the write commits, oldest first.
func mergeDeliveredHistory(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path string, prs, perPR int) []string {
	t.Helper()
	writes := []string{writeExplainFact(t, ctx, ri, path, "Slot v0", 0.5, nil)}
	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(ctx, "main", explainTestBranch))
	})
	for k := range prs {
		for j := range perPR {
			writes = append(writes, writeExplainFact(t, ctx, ri, path, fmt.Sprintf("Slot pr%d w%d", k, j), 0.5, nil))
		}
		ri.WithRead(func(svc *store.Service) {
			_, err := svc.Facts().WriteFact(ctx, "main", fmt.Sprintf("kb/other%d.md", k),
				mustSerialize(t, fmt.Sprintf("kb/other%d.md", k), "Other"), "other", "")
			require.NoError(t, err)
			require.NoError(t, svc.Branches().MergeBranch(ctx, explainTestBranch, "main", store.StrategyLocalWins))
			require.NoError(t, svc.Branches().MergeBranch(ctx, "main", explainTestBranch, store.StrategyLocalWins))
		})
	}
	return writes
}

func mustSerialize(t *testing.T, path, title string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = title + " body text."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.5
	f.Entities = []string{}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return content
}

// TestExplain_HistoryCursorWalksMergeDeliveredWrites is the regression for the
// history walk that listed one PR merge per PR, hid every write behind it and
// ended with more_available:false. Following history_cursor from the first
// call must return exactly the write commits, newest first, ending on the
// creation — and more_available must be false only on the last page.
func TestExplain_HistoryCursorWalksMergeDeliveredWrites(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	const path = "kb/slot.md"

	writes := mergeDeliveredHistory(t, ctx, ri, path, 8, 3) // 25 changes: more than one history page
	want := make([]string, len(writes))
	for i, w := range writes {
		want[len(writes)-1-i] = w
	}

	text, isErr := callExplain(t, ctx, map[string]any{"file": path})
	require.False(t, isErr, text)
	var first expResp
	require.NoError(t, json.Unmarshal([]byte(text), &first))
	root := findExpFact(first.Facts, path)
	require.NotNil(t, root)
	require.NotNil(t, root.History)
	require.Len(t, root.History.Revisions, explainHistoryDisplay)
	require.True(t, root.History.MoreAvailable)
	require.NotEmpty(t, root.History.HistoryCursor)

	// The root's own commit stays the first-parent resolved version (the last
	// PR merge), even though the newest CHANGE is the write behind it.
	var live []store.RevisionMeta
	ri.WithRead(func(svc *store.Service) {
		head, err := svc.Branches().HeadCommit(ctx, explainTestBranch)
		require.NoError(t, err)
		live, err = svc.Search().RevisionsBefore(ctx, explainTestBranch, path, head, 1)
		require.NoError(t, err)
	})
	require.Equal(t, live[0].Commit, root.Commit)
	require.NotEqual(t, root.Commit, root.History.Revisions[0].Commit, "setup: newest change arrived through a merge")

	var got []string
	var last expRev
	for _, r := range root.History.Revisions {
		got = append(got, r.Commit)
		last = r
	}
	cursor := root.History.HistoryCursor
	pages := 0
	for cursor != "" {
		pages++
		require.Less(t, pages, 10, "history paging must terminate")
		text, isErr := callExplain(t, ctx, map[string]any{"file": path, "history_cursor": cursor})
		require.False(t, isErr, text)
		var page expHistoryPage
		require.NoError(t, json.Unmarshal([]byte(text), &page))
		require.Nil(t, page.Facts, "a history page carries no facts, bodies or graph")
		require.Equal(t, path, page.Path)
		require.NotNil(t, page.History)
		require.LessOrEqual(t, len(page.History.Revisions), explainHistoryPageSize)
		for _, r := range page.History.Revisions {
			got = append(got, r.Commit)
			last = r
		}
		if page.History.MoreAvailable {
			require.NotEmpty(t, page.History.HistoryCursor)
		} else {
			require.Empty(t, page.History.HistoryCursor)
		}
		cursor = page.History.HistoryCursor
	}
	require.GreaterOrEqual(t, pages, 2, "setup: the history spans more than one history page")
	require.Equal(t, want, got, "exactly the change commits, newest first, no merges, no gaps, no repeats")
	require.Equal(t, "added", last.Action, "a complete walk ends on the creation")
}

// TestExplain_HistoryRevisionCarriesAction: every revision states whether it
// created the fact or modified it.
func TestExplain_HistoryRevisionCarriesAction(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeExplainFact(t, ctx, ri, "kb/a.md", "A v1", 0.5, nil)
	writeExplainFact(t, ctx, ri, "kb/a.md", "A v2", 0.6, nil)

	root := findExpFact(explainAll(t, ctx, "kb/a.md", ""), "kb/a.md")
	require.NotNil(t, root)
	require.Len(t, root.History.Revisions, 2)
	require.Equal(t, "modified", root.History.Revisions[0].Action)
	require.Equal(t, "added", root.History.Revisions[1].Action)
	require.Empty(t, root.History.HistoryCursor, "no cursor when the history is complete")
}

// TestExplain_HistoryCursorRefusals: a cursor only pages the file it came
// from, and is never combined with commit or cursor.
func TestExplain_HistoryCursorRefusals(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	var c string
	for i := range 5 {
		c = writeExplainFact(t, ctx, ri, "kb/a.md", fmt.Sprintf("A v%d", i), 0.5, nil)
	}
	writeExplainFact(t, ctx, ri, "kb/b.md", "B", 0.5, nil)

	root := findExpFact(explainAll(t, ctx, "kb/a.md", ""), "kb/a.md")
	require.NotNil(t, root)
	hc := root.History.HistoryCursor
	require.NotEmpty(t, hc)

	for _, args := range []map[string]any{
		{"file": "kb/b.md", "history_cursor": hc},
		{"file": "kb/a.md", "history_cursor": hc, "commit": c},
		{"file": "kb/a.md", "history_cursor": hc, "cursor": "x"},
		{"file": "kb/a.md", "history_cursor": "not-a-cursor"},
	} {
		text, isErr := callExplain(t, ctx, args)
		require.True(t, isErr, "must refuse %v: %s", args, text)
	}
}
