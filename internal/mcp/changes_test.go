package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
)

type changesOut struct {
	Changes  []store.PathChange `json:"changes"`
	Repo     string             `json:"repo"`
	Branch   string             `json:"branch"`
	Head     string             `json:"head"`
	Bookmark string             `json:"bookmark"`
	HasMore  bool               `json:"has_more"`
	Cursor   *string            `json:"cursor"`
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

// newChangesRepo is newLearnTestRepo with the consensus branch named
// explicitly, so a test can prove consensus:true is not a hardcoded "main".
func newChangesRepo(t *testing.T, consensus string) *repos.RepoInstance {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepoWithUpstream(map[string]string{}, consensus, "agent/test"))
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "named-" + consensus, UID: nextTestRepoUID(), AgentBranch: "agent/test",
		Svc: svc, Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
}

// newSubscribedChangesRepo is a subscription: no agent branch, read at the
// upstream it follows.
func newSubscribedChangesRepo(t *testing.T) *repos.RepoInstance {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "followed", UID: nextTestRepoUID(), Subscribed: true, ReadBranch: "main",
		Svc: svc, Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	})
}

func id12(ri *repos.RepoInstance) string { return federate.ID12(ri.ID()) }

// The default is the BOUND branch: an agent's own write appears at once,
// before it reaches main. The response names the repo and branch it read and
// hands back a bookmark carrying the repo.
func TestChangesHandler_DefaultReadsTheBoundBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")

	since := writeOn(t, svc, "agent/test", "kb/tasks/a/seed.md")
	writeOn(t, svc, "main", "kb/tasks/a/only-main.md")
	writeOn(t, svc, "agent/test", "kb/tasks/a/unsynced.md")

	out, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": since})
	require.False(t, isErr, text)
	require.Equal(t, "agent/test", out.Branch)
	require.Equal(t, id12(ri), out.Repo)
	require.Equal(t, id12(ri)+":"+out.Head, out.Bookmark)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/unsynced.md", Change: store.ChangeAdded}}, out.Changes,
		"the unsynced write is visible and rows from the write repo are bare paths")
	require.Nil(t, out.Cursor, "no cursor on the last page")

	// The bookmark is the next since, and so is a bare hash.
	writeOn(t, svc, "agent/test", "kb/tasks/a/later.md")
	for _, s := range []string{out.Bookmark, out.Head} {
		next, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": s})
		require.False(t, isErr, text)
		require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/later.md", Change: store.ChangeAdded}}, next.Changes, s)
	}
}

// consensus:true reads the repo's consensus branch — a write that has not
// reached it is absent — and names the branch it resolved.
func TestChangesHandler_ConsensusReadsUpstream(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")

	since := writeOn(t, svc, "main", "kb/tasks/a/seed.md")
	writeOn(t, svc, "main", "kb/tasks/a/on-main.md")
	writeOn(t, svc, "agent/test", "kb/tasks/a/only-agent.md")

	out, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": since, "consensus": true})
	require.False(t, isErr, text)
	require.Equal(t, "main", out.Branch)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/on-main.md", Change: store.ChangeAdded}}, out.Changes)

	// The same answer by naming the branch.
	named, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": since, "branch": "main"})
	require.False(t, isErr, text)
	require.Equal(t, out.Changes, named.Changes)
}

// consensus:true is the repo's RECORDED consensus branch, whatever it is
// named — not a hardcoded "main".
func TestChangesHandler_ConsensusResolvesANonMainName(t *testing.T) {
	ri := newChangesRepo(t, "trunk")
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")
	writeOn(t, svc, "trunk", "kb/tasks/a/on-trunk.md")

	out, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "consensus": true})
	require.False(t, isErr, text)
	require.Equal(t, "trunk", out.Branch)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/on-trunk.md", Change: store.ChangeAdded}}, out.Changes)
}

// A session in an experiment defaults to the experiment branch: its own
// experiment writes are visible, and the agent branch does not show them.
func TestChangesHandler_ExperimentBoundDefaultsToTheExperiment(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	branch := openTestExperiment(t, ri, "trial")
	b := repos.NewBindingOfRepo(ri, branch)
	writeOn(t, svc, branch, "kb/tasks/a/in-exp.md")

	out, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a"})
	require.False(t, isErr, text)
	require.Equal(t, branch, out.Branch)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/in-exp.md", Change: store.ChangeAdded}}, out.Changes)

	onAgent, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "branch": "agent/test"})
	require.False(t, isErr, text)
	require.Empty(t, onAgent.Changes)
}

// A lens reads any mount by its 12-hex id, at the branch the lens reads it
// at; rows from a mount other than the write repo are kb://-qualified, rows
// from the write repo stay bare even when it is named explicitly.
func TestChangesHandler_ReadsAMountedRepo(t *testing.T) {
	repoA := newLearnTestRepo(t, fact.CodeOntology())
	repoB := newLearnTestRepo(t, fact.CodeOntology())
	sub := newSubscribedChangesRepo(t)
	svcA, svcB, svcS := changesSvc(t, repoA), changesSvc(t, repoB), changesSvc(t, sub)
	writeOn(t, svcA, "agent/test", "kb/tasks/a/a.md")
	writeOn(t, svcB, "agent/test", "kb/tasks/a/b.md")
	writeOn(t, svcB, "main", "kb/tasks/a/b-main.md")
	writeOn(t, svcS, "main", "kb/tasks/a/s.md")

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
		repos.ReadTarget{RI: sub, Branch: "main"},
	)

	out, text, isErr := callChanges(t, b, map[string]any{"repo": id12(repoB), "prefix": "tasks/a"})
	require.False(t, isErr, text)
	require.Equal(t, id12(repoB), out.Repo)
	require.Equal(t, "agent/test", out.Branch)
	require.Equal(t, id12(repoB)+":"+out.Head, out.Bookmark)
	require.Equal(t, []store.PathChange{{Path: federate.QualifyPath(id12(repoB), "kb/tasks/a/b.md"), Change: store.ChangeAdded}}, out.Changes)

	cons, text, isErr := callChanges(t, b, map[string]any{"repo": id12(repoB), "prefix": "tasks/a", "consensus": true})
	require.False(t, isErr, text)
	require.Equal(t, "main", cons.Branch)
	require.Equal(t, []store.PathChange{{Path: federate.QualifyPath(id12(repoB), "kb/tasks/a/b-main.md"), Change: store.ChangeAdded}}, cons.Changes)

	subOut, text, isErr := callChanges(t, b, map[string]any{"repo": id12(sub), "prefix": "tasks/a"})
	require.False(t, isErr, text)
	require.Equal(t, "main", subOut.Branch, "a subscription is read at the branch it follows")
	require.Equal(t, []store.PathChange{{Path: federate.QualifyPath(id12(sub), "kb/tasks/a/s.md"), Change: store.ChangeAdded}}, subOut.Changes)

	own, text, isErr := callChanges(t, b, map[string]any{"repo": id12(repoA), "prefix": "tasks/a"})
	require.False(t, isErr, text)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/a.md", Change: store.ChangeAdded}}, own.Changes)
}

// Refusals reach the caller as tool errors with their remedy, never as an
// empty page.
func TestChangesHandler_RefusalsCarryTheirRemedy(t *testing.T) {
	repoA := newLearnTestRepo(t, fact.CodeOntology())
	repoB := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, repoA)
	headB := writeOn(t, changesSvc(t, repoB), "agent/test", "kb/tasks/a/b.md")
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	writeOn(t, svc, "agent/test", "kb/tasks/a/t1.md")
	side := writeOn(t, svc, "main", "kb/tasks/a/side.md")
	a12, b12 := id12(repoA), id12(repoB)

	for _, c := range []struct {
		name string
		args map[string]any
		says string
	}{
		{"unknown since", map[string]any{"since": "0123456789abcdef0123456789abcdef01234567"}, "omit since"},
		{"since not behind", map[string]any{"since": side}, "not behind"},
		{"private prefix", map[string]any{"prefix": ".knomit/inbox"}, "private"},
		{"bad cursor", map[string]any{"cursor": "garbage"}, "cursor"},
		{"repo by name", map[string]any{"repo": "test"}, `repo must be a 12-hex id from knomit_repos, not "test"`},
		{"unmounted repo", map[string]any{"repo": "0123456789ab"}, "repo 0123456789ab is not mounted in this binding"},
		{"missing branch", map[string]any{"branch": "exp/nope"}, fmt.Sprintf(`repo %s has no branch "exp/nope"`, a12)},
		{"branch and consensus", map[string]any{"branch": "main", "consensus": true}, "pass branch or consensus, not both"},
		{"kb:// prefix", map[string]any{"prefix": "kb://" + b12 + "/kb/tasks"}, fmt.Sprintf(`pass repo: %q and prefix:`, b12)},
		{"bookmark for another repo", map[string]any{"since": b12 + ":" + headB},
			fmt.Sprintf("bookmark is for repo %s (test), this call reads %s (test); pass repo: %s or omit since", b12, a12, b12)},
		{"malformed bookmark", map[string]any{"since": "test:" + headB}, "neither a bookmark"},
	} {
		_, text, isErr := callChanges(t, b, c.args)
		require.True(t, isErr, "%s: %s", c.name, text)
		require.Contains(t, text, c.says, c.name)
	}
}

// Paging through the tool: the cursor carries the whole comparison, and page
// 2 compares the SAME two commits even after the branch moves.
func TestChangesHandler_CursorPagesTheSameComparison(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")
	ctx := context.Background()

	since := writeOn(t, svc, "agent/test", "kb/other/seed.md")
	files := map[string]string{}
	for i := 0; i < 101; i++ {
		p := fmt.Sprintf("kb/tasks/a/t%03d.md", i)
		files[p] = "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# " + p + "\n\nb\n"
	}
	_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test", files, nil, "bulk", "learn")
	require.NoError(t, err)

	p1, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": since, "limit": 500})
	require.False(t, isErr, text)
	require.Len(t, p1.Changes, 100, "limit is capped at 100")
	require.True(t, p1.HasMore)
	require.NotNil(t, p1.Cursor)

	writeOn(t, svc, "agent/test", "kb/tasks/a/zz-late.md") // lands between pages

	// The cursor alone: prefix and since given here must be ignored.
	p2, text, isErr := callChanges(t, b, map[string]any{"cursor": *p1.Cursor, "prefix": "elsewhere", "since": ""})
	require.False(t, isErr, text)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/t100.md", Change: store.ChangeAdded}}, p2.Changes)
	require.False(t, p2.HasMore)
	require.Nil(t, p2.Cursor)
	require.Equal(t, p1.Head, p2.Head)
	require.Equal(t, "agent/test", p2.Branch)

	// The bookmark idiom: the finished read's bookmark is the next since.
	p3, text, isErr := callChanges(t, b, map[string]any{"prefix": "tasks/a", "since": p2.Bookmark})
	require.False(t, isErr, text)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/zz-late.md", Change: store.ChangeAdded}}, p3.Changes)
}

// A cursor records its repo and branch: a cursor minted on a read mount pages
// that mount (rows stay qualified, After stays repo-relative) even when passed
// alone; a cursor contradicted by an explicit repo or branch, or naming a repo
// this binding does not mount, is refused.
func TestChangesHandler_CursorRecordsRepoAndBranch(t *testing.T) {
	repoA := newLearnTestRepo(t, fact.CodeOntology())
	repoB := newLearnTestRepo(t, fact.CodeOntology())
	svcB := changesSvc(t, repoB)
	for _, p := range []string{"kb/tasks/a/t1.md", "kb/tasks/a/t2.md", "kb/tasks/a/t3.md"} {
		writeOn(t, svcB, "main", p)
	}
	lens := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	b12 := id12(repoB)

	p1, text, isErr := callChanges(t, lens, map[string]any{"repo": b12, "consensus": true, "prefix": "tasks/a", "limit": 1})
	require.False(t, isErr, text)
	require.True(t, p1.HasMore)
	p2, text, isErr := callChanges(t, lens, map[string]any{"cursor": *p1.Cursor, "limit": 1})
	require.False(t, isErr, text)
	require.Equal(t, b12, p2.Repo)
	require.Equal(t, "main", p2.Branch)
	require.Equal(t, []store.PathChange{{Path: federate.QualifyPath(b12, "kb/tasks/a/t2.md"), Change: store.ChangeAdded}}, p2.Changes)

	_, text, isErr = callChanges(t, lens, map[string]any{"cursor": *p1.Cursor, "repo": id12(repoA)})
	require.True(t, isErr, text)
	require.Contains(t, text, "cursor is for repo "+b12)

	_, text, isErr = callChanges(t, lens, map[string]any{"cursor": *p1.Cursor, "branch": "agent/test"})
	require.True(t, isErr, text)
	require.Contains(t, text, `cursor is for branch "main"`)

	alone := repos.NewBindingOfRepo(repoA, "agent/test")
	_, text, isErr = callChanges(t, alone, map[string]any{"cursor": *p1.Cursor})
	require.True(t, isErr, text)
	require.Contains(t, text, "cursor is for repo "+b12+", which is not mounted in this binding; restart without a cursor")
}

// A cursor minted before cursors recorded their scope keeps its old meaning:
// the write repo's consensus branch, whatever the binding is bound to.
func TestChangesHandler_LegacyCursorPagesWriteRepoConsensus(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	b := repos.NewBindingOfRepo(ri, "agent/test")
	writeOn(t, svc, "main", "kb/tasks/a/t1.md")
	head := writeOn(t, svc, "main", "kb/tasks/a/t2.md")
	writeOn(t, svc, "agent/test", "kb/tasks/a/t3-agent.md")

	legacy := store.EncodeChangesCursor(store.ChangesScope{}, "", head, "tasks/a", "kb/tasks/a/t1.md")
	out, text, isErr := callChanges(t, b, map[string]any{"cursor": legacy})
	require.False(t, isErr, text)
	require.Equal(t, "main", out.Branch)
	require.Equal(t, []store.PathChange{{Path: "kb/tasks/a/t2.md", Change: store.ChangeAdded}}, out.Changes)
}

// No write path: a read creates, moves or deletes no branch ref and no
// branches-table row, on ANY branch of ANY mount — not only the ones the
// fixture wrote to. (The store-level TestChangesUnder_WritesNothing also pins
// the commit-log tables; this one covers what the MCP handler itself could
// reach.)
func TestChangesHandler_WritesNothing(t *testing.T) {
	repoA := newLearnTestRepo(t, fact.CodeOntology())
	repoB := newLearnTestRepo(t, fact.CodeOntology())
	svc, svcB := changesSvc(t, repoA), changesSvc(t, repoB)
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	since := writeOn(t, svc, "main", "kb/tasks/a/t1.md")
	writeOn(t, svc, "main", "kb/tasks/a/t2.md")
	side := writeOn(t, svc, "agent/test", "kb/tasks/a/t3.md")
	sinceB := writeOn(t, svcB, "agent/test", "kb/tasks/a/b1.md")
	writeOn(t, svcB, "agent/test", "kb/tasks/a/b2.md")
	b12 := id12(repoB)

	snapshot := func(svc *store.Service) map[string]string {
		ctx := context.Background()
		m := map[string]string{}
		plain, agents, _ := svc.Branches().BranchInfo("")
		for _, br := range append(plain, agents...) {
			h, err := svc.Branches().HeadCommit(ctx, br)
			require.NoError(t, err)
			m["ref:"+br] = h
		}
		rows, err := svc.Branches().ListBranches(ctx)
		require.NoError(t, err)
		for _, r := range rows {
			m["row:"+r.Name] = r.GitRef
		}
		return m
	}
	before, beforeB := snapshot(svc), snapshot(svcB)
	require.Contains(t, before, "ref:main")
	require.Contains(t, before, "ref:agent/test")
	for _, args := range []map[string]any{
		{"prefix": "tasks/a"},
		{"prefix": "tasks/a", "consensus": true, "since": since},
		{"prefix": "tasks/a", "since": since, "limit": 1},
		{"prefix": "tasks/a", "since": side},
		{"prefix": "tasks/a", "since": "0123456789abcdef0123456789abcdef01234567"},
		{"prefix": "tasks/a", "branch": "exp/nope"},
		{"repo": b12, "prefix": "tasks/a", "since": sinceB, "limit": 1},
		{"repo": b12, "consensus": true},
		{"repo": b12, "branch": "exp/nope"},
		{"repo": b12, "since": id12(repoA) + ":" + since},
		{"cursor": "garbage"},
	} {
		callChanges(t, b, args)
	}
	require.Equal(t, before, snapshot(svc))
	require.Equal(t, beforeB, snapshot(svcB))
}
