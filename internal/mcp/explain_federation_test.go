package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/federate"
	"knomit/internal/repos"
)

// explainAllVia drives knomit_explain through a binding from the first call
// across every cursor page, accumulating all returned facts. commit is
// optional (anchor).
func explainAllVia(t *testing.T, b *repos.Binding, file, commit string) ([]expFact, string) {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), b)

	var all []expFact
	args := map[string]any{"file": file}
	if commit != "" {
		args["commit"] = commit
	}
	var rawFirst string
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args

	for {
		result, err := ExplainHandler()(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, result)
		text := resultText(t, result)
		require.False(t, result.IsError, "explain error: %s", text)
		if rawFirst == "" {
			rawFirst = text
		}

		var resp expResp
		require.NoError(t, json.Unmarshal([]byte(text), &resp), "bad JSON: %s", text)
		all = append(all, resp.Facts...)

		if !resp.HasMore || resp.Cursor == nil {
			break
		}
		var next mcpgo.CallToolRequest
		next.Params.Arguments = map[string]any{"file": file, "cursor": *resp.Cursor}
		req = next
	}
	return all, rawFirst
}

// TestExplainFederation_QualifiedFileRoutesToMount: a lens over write repo A +
// read repo B, explaining a kb://-qualified path, returns B's fact and every
// entry path in the response (root and, across cursor pages, every summary
// node) carries the kb://<federate.ID12(B)>/ prefix. Refs in the root body/frontmatter
// are returned exactly as stored (unqualified).
func TestExplainFederation_QualifiedFileRoutesToMount(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, ctxB := fedRepo(t)

	// Root in B references two children so the walk pages (children come on a
	// resumed page, exercising resume routing too).
	writeExplainFact(t, ctxB, repoB, "kb/child1.md", "Child1", 0.80, nil)
	writeExplainFact(t, ctxB, repoB, "kb/child2.md", "Child2", 0.70, nil)
	writeExplainFact(t, ctxB, repoB, "kb/root.md", "RootB", 0.90, []string{"kb/child1.md", "kb/child2.md"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	prefix := federate.QualifyPath(federate.ID12(repoB.ID()), "")
	file := federate.QualifyPath(federate.ID12(repoB.ID()), "kb/root.md")

	facts, _ := explainAllVia(t, b, file, "")
	require.NotEmpty(t, facts)

	// Every entry (root + summaries across pages) is qualified to B.
	for _, f := range facts {
		require.Truef(t, strings.HasPrefix(f.Path, prefix),
			"every explain entry must carry the kb://<federate.ID12(B)>/ prefix: %s", f.Path)
	}

	root := findExpFact(facts, file)
	require.NotNil(t, root, "root must appear under its qualified path: %+v", facts)
	require.Equal(t, "RootB", root.Title)
	require.Equal(t, 0, root.Depth)

	// Both children surface, qualified.
	require.NotNil(t, findExpFact(facts, federate.QualifyPath(federate.ID12(repoB.ID()), "kb/child1.md")))
	require.NotNil(t, findExpFact(facts, federate.QualifyPath(federate.ID12(repoB.ID()), "kb/child2.md")))

	// Refs in the root are returned exactly as stored — never rewritten to
	// qualified form.
	require.NotNil(t, root.Refs)
	require.ElementsMatch(t, []string{"kb/child1.md", "kb/child2.md"}, root.Refs.Local,
		"refs must be returned as stored (unqualified)")
}

// TestExplainFederation_UnqualifiedFileIsWriteRepo: a bare path resolves in the
// write repo (A); no kb:// appears anywhere in the response.
func TestExplainFederation_UnqualifiedFileIsWriteRepo(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, _ := fedRepo(t)

	writeExplainFact(t, ctxA, repoA, "kb/child.md", "ChildA", 0.80, nil)
	writeExplainFact(t, ctxA, repoA, "kb/root.md", "RootA", 0.90, []string{"kb/child.md"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)

	facts, first := explainAllVia(t, b, "kb/root.md", "")
	require.NotContains(t, first, federate.KBScheme, "unqualified explain output must never be kb://-qualified")

	root := findExpFact(facts, "kb/root.md")
	require.NotNil(t, root, "root must appear under its bare path")
	require.Equal(t, "RootA", root.Title)
	require.NotNil(t, findExpFact(facts, "kb/child.md"), "child must appear bare")
	for _, f := range facts {
		require.NotContainsf(t, f.Path, federate.KBScheme, "write-repo entry must be bare: %s", f.Path)
	}
}

// TestExplainFederation_UnmountedIDErrors: a qualified file naming a repo not in
// the binding fails loudly with "not mounted".
func TestExplainFederation_UnmountedIDErrors(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, _ := fedRepo(t)
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	ctx := repos.WithBinding(context.Background(), b)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": "kb://ffffffffffff/kb/x.md"}
	result, err := ExplainHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "unmounted qualified file must error")
	require.Contains(t, resultText(t, result), "not mounted")
}

// TestExplainFederation_LensOfOneUnchanged: a lens-of-one binding produces
// output with no kb:// anywhere — byte-compatible with today's single-repo
// envelope (the existing suite pins the rest of the shape).
func TestExplainFederation_LensOfOneUnchanged(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	writeExplainFact(t, ctxA, repoA, "kb/child.md", "ChildA", 0.80, nil)
	writeExplainFact(t, ctxA, repoA, "kb/root.md", "RootA", 0.90, []string{"kb/child.md"})

	b := repos.NewBindingForTest(repoA, repos.ReadTarget{RI: repoA, Branch: "agent/test"})

	facts, first := explainAllVia(t, b, "kb/root.md", "")
	require.NotContains(t, first, federate.KBScheme, "lens-of-one output must never be kb://-qualified")
	for _, f := range facts {
		require.NotContainsf(t, f.Path, federate.KBScheme, "lens-of-one entry must be bare: %s", f.Path)
	}
	require.NotNil(t, findExpFact(facts, "kb/root.md"))
	require.NotNil(t, findExpFact(facts, "kb/child.md"))
}
