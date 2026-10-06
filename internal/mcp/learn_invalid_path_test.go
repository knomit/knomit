package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// #384: knomit_learn with a segment that becomes a tree path go-git refuses
// to read back (a control character, git~1, a backslash-dot component, a
// zero-width-joined .git) committed it, moved the ref, and then failed the
// index sync. Every later write on the branch did the same, so the index
// froze. The store now refuses the path before the ref moves.

func invalidPathLearnReq(moment string, fact map[string]any) mcpgo.CallToolRequest {
	f := map[string]any{"body": "b " + moment, "confidence": 0.8, "sources": 1}
	for k, v := range fact {
		f[k] = v
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": moment, "facts": []any{f}}
	return req
}

func TestLearn_UnreadablePathSegment_RefusedBeforeCommit(t *testing.T) {
	cases := map[string]struct {
		fact map[string]any
		raw  string // the offending raw segment; must not reach the message unquoted
	}{
		"category line feed": {map[string]any{"topic": "architecture", "category": "a\nb", "title": "T"}, "a\nb"},
		"topic line feed":    {map[string]any{"topic": "arch\nitecture", "category": "cat", "title": "T"}, "arch\nitecture"},
		"path line feed":     {map[string]any{"path": "artifacts/jobs/a\nb/x.md", "title": "T"}, "a\nb"},
		"category git~1":     {map[string]any{"topic": "architecture", "category": "x/git~1", "title": "T"}, ""},
		"category a\\.":      {map[string]any{"topic": "architecture", "category": `x/a\.`, "title": "T"}, ""},
		"category zwj .git":  {map[string]any{"topic": "architecture", "category": "x/‍.git", "title": "T"}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ri := newLearnTestRepo(t, nil)
			ctx := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

			first, err := LearnHandler()(ctx, invalidPathLearnReq("m0",
				map[string]any{"topic": "architecture", "category": "first/cat", "title": "First"}))
			require.NoError(t, err)
			require.False(t, first.IsError, resultText(t, first))

			before := headOf(t, ri, "agent/test")
			res, err := LearnHandler()(ctx, invalidPathLearnReq("m", tc.fact))
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Equal(t, before, headOf(t, ri, "agent/test"), "a refused learn must not move the ref")
			text := resultText(t, res)
			require.Contains(t, text, "invalid path")
			require.NotContains(t, text, "notifyCommit", "refused before the commit, not by the index sync after it")
			if tc.raw != "" {
				// The path is quoted, so the control character shows escaped.
				require.NotContains(t, text, tc.raw)
			}

			// The branch is not poisoned: the next write succeeds and is indexed.
			good, err := LearnHandler()(ctx, invalidPathLearnReq("m2",
				map[string]any{"topic": "architecture", "category": "ok/cat", "title": "Good"}))
			require.NoError(t, err)
			require.False(t, good.IsError, resultText(t, good))

			out, qres := queryFacts(t, ctx, map[string]any{"path": "kb/architecture"})
			require.False(t, qres.IsError, resultText(t, qres))
			titles := make([]string, 0, len(out.Facts))
			for _, f := range out.Facts {
				titles = append(titles, f.Title)
			}
			require.ElementsMatch(t, []string{"First", "Good"}, titles)
		})
	}
}
