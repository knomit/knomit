package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// seedFedFact writes one principle-shaped fact (kind=pragmatic, type=policy,
// entities=[designer]) with the given title/domain/refs into the repo bound to
// ctx, returning the kb-relative path it was written to.
func seedFedFact(t *testing.T, ctx context.Context, moment, category, title, domain string, refs []string) string {
	t.Helper()
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": moment,
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   category,
				"title":      title,
				"body":       "designer authored " + title + ".",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{domain},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       refsAny,
			},
		},
	}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "seed failed: %s", resultText(t, result))
	path := findPathWithPrefix(resultText(t, result), "kb/principles/"+category+"/")
	require.NotEmpty(t, path, "could not locate written fact path in %s", resultText(t, result))
	return path
}

// fedRepo builds a fresh code-ontology repo and a context bound to it (for
// seeding via LearnHandler).
func fedRepo(t *testing.T) (*repos.RepoInstance, context.Context) {
	t.Helper()
	ri := newLearnTestRepo(t, fact.CodeOntology())
	return ri, repos.WithRepoInstance(context.Background(), ri)
}

// queryVia runs QueryHandler against an explicit binding.
func queryVia(t *testing.T, b *repos.Binding, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), b)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, resultText(t, result)
}

// factByTitle finds the result row with the given title.
func factByTitle(t *testing.T, resp queryResponse, title string) factOutput {
	t.Helper()
	for _, f := range resp.Facts {
		if f.Title == title {
			return f
		}
	}
	t.Fatalf("no result row titled %q in %+v", title, resp.Facts)
	return factOutput{}
}

// TestQueryFederation_QualifiedPaths: a lens over write repo A + read repo B
// returns A rows with bare paths and B rows kb://-qualified, with refs untouched.
func TestQueryFederation_QualifiedPaths(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)

	pathA := seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", []string{"kb/decisions/a/ref.md"})
	pathB := seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", []string{"kb/decisions/b/ref.md"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "query failed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Facts, 2, "both mounts' facts must appear: %s", text)

	rowA := factByTitle(t, resp, "Alpha")
	rowB := factByTitle(t, resp, "Bravo")

	// A is the write repo → bare path, no scheme.
	require.Equal(t, pathA, rowA.File)
	require.NotContains(t, rowA.File, kbScheme)
	// B is a foreign read mount → kb://<id12(B)>/<path>.
	require.Equal(t, qualifyPath(id12(repoB.ID()), pathB), rowB.File)

	// Refs are returned exactly as stored — never rewritten to qualified form.
	require.Equal(t, []string{"kb/decisions/a/ref.md"}, rowA.Frontmatter.Refs)
	require.Equal(t, []string{"kb/decisions/b/ref.md"}, rowB.Frontmatter.Refs)
}

// TestQueryFederation_LensOfOneUnchanged: a lens-of-one produces byte-for-byte
// the same output as a direct single-repo query — no kb:// anywhere.
func TestQueryFederation_LensOfOneUnchanged(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	seedFedFact(t, ctxA, "seed-1", "mission/store", "One", "store", nil)
	seedFedFact(t, ctxA, "seed-2", "mission/ui", "Two", "ui", nil)

	// Direct single-repo path (synthesized lens-of-one).
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"type": []any{"policy"}}
	direct, err := QueryHandler()(ctxA, req)
	require.NoError(t, err)
	directText := resultText(t, direct)

	// Explicit lens-of-one binding over the same repo.
	b := repos.NewBindingForTest(repoA, repos.ReadTarget{RI: repoA, Branch: "agent/test"})
	_, lensText := queryVia(t, b, map[string]any{"type": []any{"policy"}})

	require.Equal(t, directText, lensText, "lens-of-one must be byte-for-byte identical to a direct query")
	require.NotContains(t, lensText, kbScheme, "lens-of-one output must never be kb://-qualified")
}

// TestQueryFederation_QualifiedPathFilterRestrictsMount: a kb://-qualified path
// filter restricts the query to that single mount; every returned row is
// qualified to it.
func TestQueryFederation_QualifiedPathFilterRestrictsMount(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)
	pathB := seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	filter := qualifyPath(id12(repoB.ID()), "kb/")
	result, text := queryVia(t, b, map[string]any{"path": filter})
	require.Falsef(t, result.IsError, "query failed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.NotEmpty(t, resp.Facts, "the qualified mount's facts must be returned")
	for _, f := range resp.Facts {
		require.Truef(t, strings.HasPrefix(f.File, qualifyPath(id12(repoB.ID()), "")),
			"every row must be qualified to repo B: %s", f.File)
		require.Equal(t, "Bravo", f.Title, "only B's facts may appear")
	}
	require.Equal(t, qualifyPath(id12(repoB.ID()), pathB), resp.Facts[0].File)
}

// TestQueryFederation_UnmountedPathFilterErrors: a qualified filter naming a repo
// not in the binding fails loudly.
func TestQueryFederation_UnmountedPathFilterErrors(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, _ := fedRepo(t)
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"path": qualifyPath("aaaaaaaaaaaa", "kb/")})
	require.True(t, result.IsError, "unmounted qualified filter must error")
	require.Contains(t, text, "not mounted")
}

// TestQueryFederation_MountErrorFailsLoud: any mount whose Search errors fails
// the whole call (RFC §9.1). We force the error with a read mount pinned to a
// branch that does not exist in that repo's store, so its Search returns
// ErrBranchNotFound — a lens must never silently shrink its read set.
func TestQueryFederation_MountErrorFailsLoud(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, _ := fedRepo(t)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/does-not-exist"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.True(t, result.IsError, "a failing mount must fail the whole query")
	require.Contains(t, text, "search error")
}
