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

type changesOut struct {
	Changes []store.PathChange `json:"changes"`
	Head    string             `json:"head"`
	Branch  string             `json:"branch"`
	HasMore bool               `json:"has_more"`
	Cursor  *string            `json:"cursor"`
}

func callChanges(t *testing.T, b *repos.Binding, args map[string]any) (changesOut, string, bool) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := ChangesHandler()(repos.WithBinding(context.Background(), b), req)
	require.NoError(t, err)
	text := resultText(t, res)
	var out changesOut
	if !res.IsError {
		require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	}
	return out, text, res.IsError
}

func changesSvc(t *testing.T, ri *repos.RepoInstance) *store.Service {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	t.Cleanup(release)
	return svc
}

func writeOn(t *testing.T, svc *store.Service, branch, path string) string {
	t.Helper()
	body := "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# " + path + "\n\nbody\n"
	r, err := svc.Facts().WriteFact(context.Background(), branch, path, body, "w", "learn")
	require.NoError(t, err)
	return r.CommitHash
}

// Test 7: the MCP call reads the write repo's UPSTREAM, not the binding's
// agent branch — a write that has not reached main is absent.
func TestChangesHandler_AnchorsAtUpstreamNotAgentBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")

	since := writeOn(t, svc, "main", "kb/tasks/a/seed.md")
	writeOn(t, svc, "main", "kb/tasks/a/on-main.md")
	writeOn(t, svc, "agent/test", "kb/tasks/a/only-agent.md")

	out, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": since})
	require.False(t, isErr, text)
	require.Equal(t, "main", out.Branch)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/on-main.md", Change: store.ChangeAdded}}, out.Changes)
	require.Nil(t, out.Cursor, "no cursor on the last page")
}

// Paging through the tool: the cursor carries the whole comparison, and page
// 2 compares the SAME two commits even after main moves.
func TestChangesHandler_CursorPagesTheSameComparison(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")
	ctx := context.Background()

	since := writeOn(t, svc, "main", "kb/other/seed.md")
	files := map[string]string{}
	for i := 0; i < 101; i++ {
		p := fmt.Sprintf("kb/tasks/a/t%03d.md", i)
		files[p] = "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# " + p + "\n\nb\n"
	}
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "main", files, nil, "bulk", "learn")
	require.NoError(t, err)

	p1, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": since, "limit": 500})
	require.False(t, isErr, text)
	require.Len(t, p1.Changes, 100, "limit is capped at 100")
	require.True(t, p1.HasMore)
	require.NotNil(t, p1.Cursor)

	writeOn(t, svc, "main", "kb/tasks/a/zz-late.md") // lands between pages

	// The cursor alone: prefix and since given here must be ignored.
	p2, text, isErr := callChanges(t, b, map[string]any{"cursor": *p1.Cursor, "prefix": "elsewhere", "since": ""})
	require.False(t, isErr, text)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/t100.md", Change: store.ChangeAdded}}, p2.Changes)
	require.False(t, p2.HasMore)
	require.Nil(t, p2.Cursor)
	require.Equal(t, p1.Head, p2.Head)

	// The bookmark idiom: head from the finished read is the next since.
	p3, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": p2.Head})
	require.False(t, isErr, text)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/zz-late.md", Change: store.ChangeAdded}}, p3.Changes)
}

// Refusals reach the caller as tool errors with their remedy, never as an
// empty page.
func TestChangesHandler_RefusalsCarryTheirRemedy(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")
	writeOn(t, svc, "main", "kb/tasks/a/t1.md")
	side := writeOn(t, svc, "agent/test", "kb/tasks/a/side.md")

	_, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": "0123456789abcdef0123456789abcdef01234567"})
	require.True(t, isErr)
	require.Contains(t, text, "omit since")

	_, text, isErr = callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": side})
	require.True(t, isErr, "a since from the agent branch is not behind main: %s", text)
	require.Contains(t, text, "not behind")

	_, text, isErr = callChanges(t, b, map[string]any{"prefix": ".knomit/inbox"})
	require.True(t, isErr)
	require.Contains(t, text, "private")

	_, text, isErr = callChanges(t, b, map[string]any{"cursor": "garbage"})
	require.True(t, isErr)
	require.Contains(t, text, "cursor")
}

// No write path: a read moves no ref and creates no commit on any branch.
func TestChangesHandler_WritesNothing(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")
	since := writeOn(t, svc, "main", "kb/tasks/a/t1.md")
	writeOn(t, svc, "main", "kb/tasks/a/t2.md")
	writeOn(t, svc, "agent/test", "kb/tasks/a/t3.md")

	tips := func() map[string]string {
		m := map[string]string{}
		for _, br := range []string{"main", "agent/test"} {
			h, err := svc.Branches().HeadCommit(context.Background(), br)
			require.NoError(t, err)
			m[br] = h
		}
		return m
	}
	before := tips()
	for _, args := range []map[string]any{
		{"prefix": "tasks/a"},
		{"prefix": "tasks/a", "since": since},
		{"prefix": "tasks/a", "since": "0123456789abcdef0123456789abcdef01234567"},
	} {
		callChanges(t, b, args)
	}
	require.Equal(t, before, tips())
}
