package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// firstHistoryCursor explains file at HEAD and returns the root's
// history_cursor (asserting there is one).
func firstHistoryCursor(t *testing.T, ctx context.Context, file string) string {
	t.Helper()
	root := findExpFact(explainAll(t, ctx, file, ""), fact.NormalizePath("kb", file))
	require.NotNil(t, root)
	require.NotNil(t, root.History)
	require.NotEmpty(t, root.History.HistoryCursor)
	return root.History.HistoryCursor
}

func writeVersions(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path string, n int) string {
	t.Helper()
	var c string
	for i := range n {
		c = writeExplainFact(t, ctx, ri, path, fmt.Sprintf("V%d", i), 0.5, nil)
	}
	return c
}

// TestExplain_HistoryDiffIsAgainstEditedFromContent is the regression for
// review finding 2 at the tool boundary: fork at B (confidence 0.5); X on the
// side sets 0.7; Y on the bound branch edits only the body; a merge composes
// both. Y's diff must show its body edit and NO confidence change — diffed
// against its list neighbour X it showed a fake 0.7 -> 0.5 revert.
func TestExplain_HistoryDiffIsAgainstEditedFromContent(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	const path = "kb/d.md"

	writeExplainFact(t, ctx, ri, path, "Base", 0.5, nil)
	var xCommit string
	ri.WithRead(func(svc *store.Service) {
		exp, err := svc.Experiments().OpenExperiment(ctx, "side", "", explainTestBranch)
		require.NoError(t, err)
		r, err := svc.Facts().WriteFact(ctx, exp.Branch(), path, mustSerializeConf(t, path, "Base", 0.7), "X: confidence", "")
		require.NoError(t, err)
		xCommit = r.CommitHash
	})
	yCommit := writeExplainFact(t, ctx, ri, path, "Base edited", 0.5, nil)
	ri.WithRead(func(svc *store.Service) {
		_, err := svc.Experiments().CommitExperiment(ctx, "side", map[string]store.Resolution{
			path: {Body: []byte(mustSerializeConf(t, path, "Base edited", 0.7))},
		})
		require.NoError(t, err)
	})

	root := findExpFact(explainAll(t, ctx, path, ""), path)
	require.NotNil(t, root)
	revs := map[string]expRev{}
	for _, r := range root.History.Revisions {
		revs[r.Commit] = r
	}
	// The first page shows 3: the composing merge, then X and Y (both edited
	// from Base), which sit next to each other in the list.
	require.Len(t, root.History.Revisions, explainHistoryDisplay)
	require.Contains(t, revs, xCommit)
	require.Contains(t, revs, yCommit)

	var dy, dx revisionDiff
	require.NoError(t, json.Unmarshal(revs[yCommit].Diff, &dy))
	require.Empty(t, dy.Confidence, "Y did not change confidence; it was edited from Base, not from X")
	require.NotEmpty(t, dy.Body)
	require.NoError(t, json.Unmarshal(revs[xCommit].Diff, &dx))
	require.Equal(t, []float64{0.5, 0.7}, dx.Confidence, "X was edited from Base")
	require.Empty(t, dx.Body)
}

func mustSerializeConf(t *testing.T, path, title string, conf float64) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = title + " body text."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = conf
	f.Sources = 1
	f.Entities = []string{}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return content
}

// TestExplain_HistoryCursorAfterRewindIsHistoryChanged is the regression for
// review finding 4: after the branch is rewound past the cursor's ANCHOR the
// page is a distinguishable "history changed" error — not a short page and not
// an empty "complete" one. The rewind keeps every change the frontier names on
// the branch, so only the anchor check can refuse; and a cursor naming an
// unknown anchor is "unknown", not empty.
func TestExplain_HistoryCursorAfterRewindIsHistoryChanged(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeVersions(t, ctx, ri, "kb/r.md", 7)
	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(ctx, "old", explainTestBranch))
	})
	writeVersions(t, ctx, ri, "kb/r.md", 1) // the anchor: the only commit the rewind drops
	hc := firstHistoryCursor(t, ctx, "kb/r.md")

	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().DropBranch(ctx, explainTestBranch))
		require.NoError(t, svc.Branches().CreateBranch(ctx, explainTestBranch, "old"))
	})
	text, isErr := callExplain(t, ctx, map[string]any{"file": "kb/r.md", "history_cursor": hc})
	require.True(t, isErr, text)
	require.Contains(t, text, "history changed")

	c, ok := decodeHistoryCursor(hc)
	require.True(t, ok)
	c.Anchor = strings.Repeat("0f", 20)
	text, isErr = callExplain(t, ctx, map[string]any{"file": "kb/r.md", "history_cursor": encodeHistoryCursor(c)})
	require.True(t, isErr, text)
	require.Contains(t, text, "unknown history_cursor")
}

// TestExplain_HistoryCursorFrontierOffBranchIsHistoryChanged: the anchor is
// still on the branch, but the position names a change visible only on
// another branch, so only the frontier check can refuse.
func TestExplain_HistoryCursorFrontierOffBranchIsHistoryChanged(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeVersions(t, ctx, ri, "kb/f.md", 6)
	var side string
	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(ctx, "side", explainTestBranch))
		r, err := svc.Facts().WriteFact(ctx, "side", "kb/f.md", mustSerializeConf(t, "kb/f.md", "Side", 0.5), "side", "")
		require.NoError(t, err)
		side = r.CommitHash
	})
	c, ok := decodeHistoryCursor(firstHistoryCursor(t, ctx, "kb/f.md"))
	require.True(t, ok)
	c.Frontier = []string{side}
	text, isErr := callExplain(t, ctx, map[string]any{"file": "kb/f.md", "history_cursor": encodeHistoryCursor(c)})
	require.True(t, isErr, text)
	require.Contains(t, text, "history changed")
}

// TestExplain_HistoryCursorBoundToBindingAndFactPaths is the regression for
// review finding 6: a cursor is refused under another binding, for a path
// that is not a fact path, and for a fact that is not readable at its anchor.
func TestExplain_HistoryCursorBoundToBindingAndFactPaths(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeVersions(t, ctx, ri, "kb/s.md", 6)
	hc := firstHistoryCursor(t, ctx, "kb/s.md")

	// Same store, different binding (another repo instance over it).
	var svc *store.Service
	ri.WithRead(func(s *store.Service) { svc = s })
	other := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "other", UID: nextTestRepoUID(), AgentBranch: explainTestBranch,
		Svc: svc, Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
	otherCtx := repos.WithRepoInstance(context.Background(), other)
	text, isErr := callExplain(t, otherCtx, map[string]any{"file": "kb/s.md", "history_cursor": hc})
	require.True(t, isErr, text)
	require.Contains(t, text, "unknown history_cursor")

	// A hand-built cursor for a non-fact path is refused before any lookup.
	c, ok := decodeHistoryCursor(hc)
	require.True(t, ok)
	c.Path = ".github/notes.md"
	text, isErr = callExplain(t, ctx, map[string]any{"file": ".github/notes.md", "history_cursor": encodeHistoryCursor(c)})
	require.True(t, isErr, text)
	require.Contains(t, text, "not a fact path")

	// A cursor for a fact path that is not readable at the anchor is unknown.
	c, _ = decodeHistoryCursor(hc)
	c.Path = "kb/never-written.md"
	text, isErr = callExplain(t, ctx, map[string]any{"file": "kb/never-written.md", "history_cursor": encodeHistoryCursor(c)})
	require.True(t, isErr, text)
	require.Contains(t, text, "unknown history_cursor")
}

// TestExplain_HistoryCursorComparesNormalizedPaths is the regression for
// review finding 8: the cursor names the NORMALIZED path, so the same fact
// addressed as kb/n or kb/n.md pages the same history.
func TestExplain_HistoryCursorComparesNormalizedPaths(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeVersions(t, ctx, ri, "kb/n.md", 6)
	hc := firstHistoryCursor(t, ctx, "kb/n.md")

	for _, file := range []string{"kb/n", "n", "kb/n.md"} {
		text, isErr := callExplain(t, ctx, map[string]any{"file": file, "history_cursor": hc})
		require.False(t, isErr, "%s: %s", file, text)
	}
}
