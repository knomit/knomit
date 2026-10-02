package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// #361: a dedup merge counts the incoming sources only for a novel ref.
func TestMergeFacts_SourcesOnlyOnNovelRef(t *testing.T) {
	h := func(c string) string { return strings.Repeat(c, 40) }
	src := func(commit, blob, frag string) string {
		return "src://aaaaaaaaaaaa/internal/x.go@" + commit + ":" + blob + frag
	}
	cases := []struct {
		name           string
		inRefs, exRefs []string
		inType         fact.Type
		inConf         float64
		want           int
	}{
		{"same URL", []string{"https://e.org/a"}, []string{"https://e.org/a"}, fact.Observation, 0.5, 2},
		{"novel URL", []string{"https://e.org/b"}, []string{"https://e.org/a"}, fact.Observation, 0.5, 5},
		{"novel URL, incoming wins", []string{"https://e.org/b"}, []string{"https://e.org/a"}, fact.Observation, 0.99, 5},
		{"same URL, incoming wins", []string{"https://e.org/a"}, []string{"https://e.org/a"}, fact.Observation, 0.99, 2},
		{"bare vs canonical kb", []string{"kb/t/o.md"}, []string{"kb://" + testLocalID + "/kb/t/o.md"}, fact.Observation, 0.5, 2},
		{"src same blob other commit", []string{src(h("2"), h("a"), "")}, []string{src(h("1"), h("a"), "")}, fact.Observation, 0.5, 2},
		{"src other blob", []string{src(h("1"), h("b"), "")}, []string{src(h("1"), h("a"), "")}, fact.Observation, 0.5, 5},
		{"src other line range", []string{src(h("1"), h("a"), "#L1-L2")}, []string{src(h("1"), h("a"), "#L8-L9")}, fact.Observation, 0.5, 2},
		{"URL fragment", []string{"https://e.org/a#x"}, []string{"https://e.org/a"}, fact.Observation, 0.5, 2},
		{"URL trailing slash", []string{"https://e.org/a/"}, []string{"https://e.org/a"}, fact.Observation, 0.5, 2},
		{"self ref", []string{"kb/tech/foo.md"}, nil, fact.Observation, 0.5, 2},
		{"ref-less pair", nil, nil, fact.Observation, 0.5, 2},
		{"ref-less incoming", nil, []string{"https://e.org/a"}, fact.Observation, 0.5, 2},
		{"incoming hypothesis", []string{"https://e.org/b"}, []string{"https://e.org/a"}, fact.Hypothesis, 0.5, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := fact.NewFact("kb/tech/foo.md")
			ex.Title, ex.Body, ex.Type = "E", "eb", fact.Observation
			ex.Confidence, ex.Sources, ex.Refs = 0.9, 2, tc.exRefs
			in := fact.NewFact("kb/tech/new.md")
			in.Title, in.Body, in.Type = "N", "nb", tc.inType
			in.Confidence, in.Sources, in.Refs = tc.inConf, 3, tc.inRefs
			require.Equal(t, tc.want, mergeFacts(in, ex, testLocalID).Sources)
		})
	}
}

func refNoveltyReq(refs ...any) mcpgo.CallToolRequest {
	r := motifLearnReq("reread", "the toolchain silently falls back when the variable is unset....", 0.8, []any{})
	r.Params.Arguments.(map[string]any)["facts"].([]any)[0].(map[string]any)["refs"] = refs
	return r
}

func TestLearnHandler_RereadOfOneURLDoesNotInflateSources(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)
	const u1, u2 = "https://example.org/one", "https://example.org/two"

	var path string
	for i := 0; i < 4; i++ {
		r, err := LearnHandler(emb)(ctx, refNoveltyReq(u1))
		require.NoError(t, err)
		require.False(t, r.IsError, resultText(t, r))
		if i == 0 {
			path = mergedFactPath(t, r)
			require.NotContains(t, resultText(t, r), "sources unchanged")
			continue
		}
		require.Equal(t, path, mergedFactPath(t, r), "must merge")
		var parsed struct{ Notes []string }
		require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &parsed))
		require.Len(t, parsed.Notes, 1)
		require.Contains(t, parsed.Notes[0], "no new refs, sources unchanged")
		require.Contains(t, parsed.Notes[0], path)
	}
	require.Equal(t, 1, readFactAt(t, riFrom(t, ctx), path).Sources)

	r, err := LearnHandler(emb)(ctx, refNoveltyReq(u2))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	require.Equal(t, path, mergedFactPath(t, r))
	require.NotContains(t, resultText(t, r), "sources unchanged")
	got := readFactAt(t, riFrom(t, ctx), path)
	require.Equal(t, 2, got.Sources)
	require.ElementsMatch(t, []string{u1, u2}, got.Refs)
}
