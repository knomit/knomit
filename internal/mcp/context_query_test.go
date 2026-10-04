package mcp

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// seedVerdicts learns three verdicts for t-17 (two disagree) and one for t-18
// under kb/verdicts, plus a note for t-17 elsewhere, through knomit_learn.
func seedVerdicts(t *testing.T, ctx context.Context, tag string) {
	t.Helper()
	v := func(cat, title, task, verdict string) map[string]any {
		return ctxLearnItem("verdicts", cat, tag+" "+title, map[string]any{"task": task, "verdict": verdict})
	}
	res, err := LearnHandler()(ctx, learnItems(
		v("t-17", "A", "t-17", "disagree"),
		v("t-17", "B", "t-17", "disagree"),
		v("t-17", "C", "t-17", "agree"),
		v("t-18", "D", "t-18", "disagree"),
		ctxLearnItem("notes", "x", tag+" Note", map[string]any{"task": "t-17"}),
	))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
}

func titlesOf(resp queryResponse) []string {
	out := make([]string, 0, len(resp.Facts))
	for _, f := range resp.Facts {
		out = append(out, f.Title)
	}
	sort.Strings(out)
	return out
}

// knomit_query's context filter on a repo binding: exact counts, combined with
// path; the rows carry their context; a context-only query is a query; a bad
// key is refused rather than silently matching nothing.
//
// SABOTAGE: drop `Context: ctxFilter` from parseQueryFilters → 4/5 rows → red.
func TestQuery_ContextFilter(t *testing.T) {
	_, _, ctx, _ := newContextRepo(t, contextOntologyYAML)
	seedVerdicts(t, ctx, "r")

	resp, res := queryFacts(t, ctx, map[string]any{"path": "kb/verdicts", "context": map[string]any{"task": "t-17"}})
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, []string{"r A", "r B", "r C"}, titlesOf(resp))

	resp, _ = queryFacts(t, ctx, map[string]any{"path": "kb/verdicts", "context": map[string]any{"task": "t-17", "verdict": "disagree"}})
	require.Equal(t, []string{"r A", "r B"}, titlesOf(resp))
	for _, f := range resp.Facts {
		require.Equal(t, map[string]any{"task": "t-17", "verdict": "disagree"}, f.Frontmatter.Context, f.Title)
	}

	resp, res = queryFacts(t, ctx, map[string]any{"context": map[string]any{"task": "t-17"}})
	require.False(t, res.IsError, "a context-only query is a query: %s", resultText(t, res))
	require.Equal(t, []string{"r A", "r B", "r C", "r Note"}, titlesOf(resp))

	resp, _ = queryFacts(t, ctx, map[string]any{"sort": "recent", "path": "kb/verdicts", "context": map[string]any{"verdict": "disagree"}})
	require.Equal(t, []string{"r A", "r B", "r D"}, titlesOf(resp), "sort=recent applies the same filter")

	_, res = queryFacts(t, ctx, map[string]any{"context": map[string]any{"Task": "t-17"}})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `key "Task"`)
	_, res = queryFacts(t, ctx, map[string]any{"context": map[string]any{"task": []any{"t-17"}}})
	require.True(t, res.IsError)
}

// C5, the cursor: page 2 onward of a context-filtered query returns only
// matches — the filter is frozen into the snapshot with the cursor.
func TestQuery_ContextFilterHoldsAcrossCursorPages(t *testing.T) {
	_, _, ctx, _ := newContextRepo(t, contextOntologyYAML)
	seedVerdicts(t, ctx, "r")

	var titles []string
	resp, res := queryFacts(t, ctx, map[string]any{"path": "kb/", "context": map[string]any{"task": "t-17"}, "limit": 1})
	require.False(t, res.IsError, resultText(t, res))
	pages := 0
	for {
		pages++
		for _, f := range resp.Facts {
			titles = append(titles, f.Title)
			require.Equal(t, "t-17", f.Frontmatter.Context["task"], "page %d: %s", pages, f.Title)
		}
		if resp.Cursor == nil {
			break
		}
		resp, res = queryFacts(t, ctx, map[string]any{"cursor": *resp.Cursor, "limit": 1})
		require.False(t, res.IsError, resultText(t, res))
	}
	sort.Strings(titles)
	require.Equal(t, []string{"r A", "r B", "r C", "r Note"}, titles)
	require.Equal(t, 4, pages, "fixture: one row per page, so pages 2..4 were served from the cursor")
}

// C5, a lens: the filter reaches every mount of the read set.
func TestQuery_ContextFilterThroughALens(t *testing.T) {
	riA, _, ctxA, _ := newContextRepo(t, contextOntologyYAML)
	riB, _, ctxB, _ := newContextRepo(t, contextOntologyYAML)
	seedVerdicts(t, ctxA, "a")
	seedVerdicts(t, ctxB, "b")

	b := repos.NewBindingForTest(riA,
		repos.ReadTarget{RI: riA, Branch: "agent/test"},
		repos.ReadTarget{RI: riB, Branch: "agent/test"},
	)
	for _, sortMode := range []string{"", "recent"} {
		args := map[string]any{"path": "kb/verdicts", "context": map[string]any{"task": "t-17", "verdict": "disagree"}}
		if sortMode != "" {
			args["sort"] = sortMode
		}
		res, text := queryVia(t, b, args)
		require.False(t, res.IsError, text)
		var resp queryResponse
		require.NoError(t, json.Unmarshal([]byte(text), &resp))
		require.Equal(t, []string{"a A", "a B", "b A", "b B"}, titlesOf(resp), "sort=%q: both mounts, only the matches", sortMode)
	}
}
