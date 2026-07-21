package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// RFC §6.2: the server never rewrites ref content on write or read. A kb:// ref
// an agent copies out of a query result is stored and returned VERBATIM —
// byte-for-byte, same order — through learn, query, explain, and update. These
// are pinning tests: they prove existing behaviour, and any failure is a real
// defect (not something to "fix" by rewriting production code).
//
// classifyRefs (explain.go) buckets refs Local/External: a bare "*.md" ref is a
// local fact edge, but a "kb://…/z.md" ref points into another repo, so it is
// External despite ending in ".md". That bucketing is a CLASSIFICATION choice,
// not a rewrite — the refs' string content is untouched in either bucket. The
// hard requirement asserted here is string preservation.

// learnFactVia learns a single principle-shaped fact through an explicit
// binding (write goes to the binding's write repo) and returns the kb-relative
// path it was committed to. Refs are passed through as given.
func learnFactVia(t *testing.T, b *repos.Binding, moment, category, title string, refs []string) string {
	t.Helper()
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	ctx := repos.WithBinding(context.Background(), b)
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
				"domain":     []any{"store"},
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
	require.Falsef(t, result.IsError, "learn failed: %s", resultText(t, result))
	return mergedFactPath(t, result)
}

// updateRefsVia replaces a fact's refs wholesale through an explicit binding.
func updateRefsVia(t *testing.T, b *repos.Binding, path, moment string, refs []string) {
	t.Helper()
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	ctx := repos.WithBinding(context.Background(), b)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file":        path,
		"moment_name": moment,
		"updates":     map[string]any{"refs": refsAny},
	}
	result, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "update failed: %s", resultText(t, result))
}

// TestLearn_KbRefsStoredAsGiven learns a fact through a lens(A,B) binding whose
// refs include a kb://-qualified ref pointing at read mount B and a non-.md
// external ref. It then proves the ref STRINGS survive verbatim through both
// read paths — query (include_body) preserves them exactly and in order, and
// explain classifies them without touching the strings.
func TestLearn_KbRefsStoredAsGiven(t *testing.T) {
	repoA, _ := fedRepo(t) // write repo
	repoB, _ := fedRepo(t) // foreign read mount referenced by the kb:// ref

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)

	// The kb:// ref names B's REAL mounted id — exactly what an agent copies out
	// of a federated query result. The src:// ref is a non-.md external ref.
	kbRef := qualifyPath(id12(repoB.ID()), "kb/x/y/z.md") // kb://<id12(B)>/kb/x/y/z.md
	srcRef := "src://other/path@abc123"
	given := []string{kbRef, srcRef}

	path := learnFactVia(t, b, "seed-refs", "mission/store", "RefHolder", given)

	// (a) query with include_body → frontmatter.refs EXACTLY as given, same order.
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}, "include_body": true})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	row := factByTitle(t, resp, "RefHolder")
	require.Equal(t, path, row.File, "the write-repo fact must carry a bare path")
	require.Equal(t, given, row.Frontmatter.Refs,
		"query must return refs byte-for-byte as given, same order — no rewrite (RFC §6.2)")

	// (b) explain the new fact → ref STRINGS untouched. classifyRefs sends the
	// kb://…/z.md ref to External (cross-repo pointer) and the src:// ref to
	// External too. Assert the buckets; the invariant is that every string is
	// preserved verbatim in whichever bucket it lands.
	ctx := repos.WithBinding(context.Background(), b)
	facts := explainAll(t, ctx, path, "")
	require.NotEmpty(t, facts, "explain must return the root fact")
	root := facts[0]
	require.Equal(t, "RefHolder", root.Title)
	require.NotNil(t, root.Refs, "root fact must carry classified refs")

	// HEAD buckets: kb://…/z.md → External (cross-repo pointer); src://… → External.
	require.Empty(t, root.Refs.Local,
		"no bare .md ref given — Local is empty")
	require.ElementsMatch(t, []string{kbRef, srcRef}, root.Refs.External,
		"kb://…/z.md (cross-repo) and src://… both classify as External — strings must be verbatim")

	// Belt-and-suspenders: the union of both buckets is exactly the given set,
	// same members, no rewrite of any string.
	require.ElementsMatch(t, given, append(append([]string{}, root.Refs.Local...), root.Refs.External...),
		"explain must preserve every ref string across both buckets")
}

// TestUpdate_KbRefsReplacedVerbatim seeds a fact, then replaces its refs with a
// new list containing a kb:// ref. Re-reading via both query and explain must
// return the new ref strings verbatim — update stores what the caller sends,
// never rewriting the ref content (RFC §6.2).
func TestUpdate_KbRefsReplacedVerbatim(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, _ := fedRepo(t)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)

	// Start with an unrelated external ref, then replace wholesale.
	path := learnFactVia(t, b, "seed-upd", "mission/store", "UpdHolder", []string{"src://old/ref@000000"})

	newKbRef := qualifyPath(id12(repoB.ID()), "kb/a/b/c.md")
	newSrcRef := "src://new/ref@def456"
	replacement := []string{newKbRef, newSrcRef}
	updateRefsVia(t, b, path, "swap-refs", replacement)

	// Re-read via query → exact strings, same order, old ref gone.
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}, "include_body": true})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	row := factByTitle(t, resp, "UpdHolder")
	require.Equal(t, replacement, row.Frontmatter.Refs,
		"update must replace refs with the given list verbatim, same order (RFC §6.2)")

	// Re-read via explain → same strings, classified but untouched.
	ctx := repos.WithBinding(context.Background(), b)
	facts := explainAll(t, ctx, path, "")
	require.NotEmpty(t, facts, "explain must return the root fact")
	root := facts[0]
	require.NotNil(t, root.Refs, "root fact must carry classified refs")
	require.Empty(t, root.Refs.Local,
		"no bare .md ref given — Local is empty")
	require.ElementsMatch(t, []string{newKbRef, newSrcRef}, root.Refs.External,
		"replaced kb://…/c.md (cross-repo) and src://… both classify as External — strings must be verbatim")
}
