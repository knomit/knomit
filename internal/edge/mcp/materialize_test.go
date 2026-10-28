package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// These tests drive knomit_materialize over real stores: every fact is
// written through the store, so every ref's DERIVED_FROM edge (its cited
// version) is the one the write path records. Each test asserts WHICH facts
// come back in WHICH order, not merely that something did.

type matPage struct {
	Root                string                 `json:"root"`
	Anchor              string                 `json:"anchor"`
	Order               string                 `json:"order"`
	Audience            string                 `json:"audience"`
	Unpinned            bool                   `json:"unpinned"`
	CrossKB             bool                   `json:"cross_kb"`
	Depth               int                    `json:"depth"`
	MaxNodes            int                    `json:"max_nodes"`
	Total               int                    `json:"total"`
	Truncated           materialize.Truncation `json:"truncated"`
	InstructionsVersion string                 `json:"instructions_version"`
	Instructions        string                 `json:"instructions"`
	Text                *string                `json:"text"`
	Facts               []materialize.Node     `json:"facts"`
	NFrom               int                    `json:"n_from"`
	NTo                 int                    `json:"n_to"`
	Cursor              *string                `json:"cursor"`
	HasMore             bool                   `json:"has_more"`
}

// writeMat writes one fact through the store and returns its commit. A
// frame is written as kind pragmatic, type frame.
func writeMat(t *testing.T, ri *repos.RepoInstance, path, title, body string, typ fact.Type, refs ...string) string {
	t.Helper()
	return writeMatConf(t, ri, path, title, body, typ, 0.9, refs...)
}

// writeMatConf is writeMat with an explicit confidence: a way to make a new
// version whose BODY is unchanged. (A write whose content is byte-identical
// is no new version at all: edges stay on the old blob.)
func writeMatConf(t *testing.T, ri *repos.RepoInstance, path, title, body string, typ fact.Type, conf float64, refs ...string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = body
	f.Type = typ
	if fact.PragmaticTypes[typ] {
		f.Kind = fact.Pragmatic
	}
	f.Domain = []string{"testing"}
	f.Confidence = conf
	f.Sources = 1
	f.Entities = []string{}
	f.Refs = refs
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	var commit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		r, werr := svc.Facts().WriteFact(context.Background(), explainTestBranch, path, content, "write "+path, "")
		require.NoError(t, werr)
		commit = r.CommitHash
	}))
	require.NotEmpty(t, commit)
	return commit
}

func obs(t *testing.T, ri *repos.RepoInstance, path, title string, refs ...string) string {
	t.Helper()
	return writeMat(t, ri, path, title, title+" body.", fact.Observation, refs...)
}

func frame(t *testing.T, ri *repos.RepoInstance, path, title string, refs ...string) string {
	t.Helper()
	return writeMat(t, ri, path, title, title+" frame body.", fact.Frame, refs...)
}

func retractMat(t *testing.T, ri *repos.RepoInstance, path string) string {
	t.Helper()
	var commit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		c, err := svc.Facts().DeleteFact(context.Background(), explainTestBranch, path, "retract "+path)
		require.NoError(t, err)
		commit = c
	}))
	return commit
}

// matCall makes one call and decodes the page; it fails the test on a tool
// error.
func matCall(t *testing.T, ctx context.Context, mgr *repos.Manager, args map[string]any) matPage {
	t.Helper()
	res, text := matCallRaw(t, ctx, mgr, args)
	require.False(t, res.IsError, "materialize error: %s", text)
	var p matPage
	require.NoError(t, json.Unmarshal([]byte(text), &p), text)
	return p
}

func matCallRaw(t *testing.T, ctx context.Context, mgr *repos.Manager, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := MaterializeHandler(mgr)(ctx, req)
	require.NoError(t, err)
	return res, resultText(t, res)
}

// matAll drives the first call and every cursor page.
func matAll(t *testing.T, ctx context.Context, mgr *repos.Manager, args map[string]any) []matPage {
	t.Helper()
	p := matCall(t, ctx, mgr, args)
	pages := []matPage{p}
	for p.HasMore {
		require.NotNil(t, p.Cursor)
		next := map[string]any{"cursor": *p.Cursor}
		if l, ok := args["limit"]; ok {
			next["limit"] = l
		}
		p = matCall(t, ctx, mgr, next)
		pages = append(pages, p)
	}
	return pages
}

// humanFacts materializes for the human audience and returns every fact,
// pages concatenated.
func humanFacts(t *testing.T, ctx context.Context, mgr *repos.Manager, args map[string]any) ([]materialize.Node, matPage) {
	t.Helper()
	args["audience"] = "human"
	pages := matAll(t, ctx, mgr, args)
	var out []materialize.Node
	for _, p := range pages {
		out = append(out, p.Facts...)
	}
	return out, pages[0]
}

func titles(nodes []materialize.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Title
	}
	return out
}

func nodeByTitle(t *testing.T, nodes []materialize.Node, title string) materialize.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Title == title {
			return n
		}
	}
	t.Fatalf("no node titled %q in %v", title, titles(nodes))
	return materialize.Node{}
}

func linkNs(n materialize.Node) []int {
	out := []int{}
	for _, l := range n.BuildsOn {
		out = append(out, l.N)
	}
	return out
}

func omitted(n materialize.Node) []string {
	out := []string{}
	for _, o := range n.NotIncluded {
		label := o.Title
		if label == "" {
			label = o.Path
		}
		out = append(out, label+"|"+o.Reason)
	}
	return out
}

// seedT1 writes R→[A, B], A→[C], B→[D, C], C→[R] so that every edge is
// pinned at a version that carries the cycle: drafts first, then C is
// rewritten to cite R, then A, B and R are rewritten (new content, so new
// versions) to cite the new C, A and B. R's two versions differ only in
// confidence: C cites the OLDER R, whose body is the same, so that citation
// is not marked as a different version (bodies are compared, never commits).
func seedT1(t *testing.T, ri *repos.RepoInstance) {
	obs(t, ri, "kb/t1/d.md", "D")
	writeMat(t, ri, "kb/t1/c.md", "C", "C draft.", fact.Observation)
	writeMat(t, ri, "kb/t1/a.md", "A", "A draft.", fact.Observation, "kb/t1/c.md")
	writeMat(t, ri, "kb/t1/b.md", "B", "B draft.", fact.Observation, "kb/t1/d.md", "kb/t1/c.md")
	writeMatConf(t, ri, "kb/t1/r.md", "R", "R body.", fact.Observation, 0.88, "kb/t1/a.md", "kb/t1/b.md")
	obs(t, ri, "kb/t1/c.md", "C", "kb/t1/r.md")
	obs(t, ri, "kb/t1/a.md", "A", "kb/t1/c.md")
	obs(t, ri, "kb/t1/b.md", "B", "kb/t1/d.md", "kb/t1/c.md")
	obs(t, ri, "kb/t1/r.md", "R", "kb/t1/a.md", "kb/t1/b.md")
}

// T1: pre-order depth-first in the order refs are WRITTEN, each fact once,
// later citations (diamond C, cycle C→R) as back-references.
func TestMaterialize_RefsOrderBackRefsAndCycle(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedT1(t, ri)

	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "depth": 2})
	require.Equal(t, []string{"R", "A", "C", "B", "D"}, titles(nodes), "pre-order DFS in refs order")
	require.Equal(t, []int{0, 1, 2, 1, 2}, []int{nodes[0].Depth, nodes[1].Depth, nodes[2].Depth, nodes[3].Depth, nodes[4].Depth})
	require.Equal(t, "root", nodes[0].Role)
	require.Equal(t, "refs", first.Order)

	b := nodeByTitle(t, nodes, "B")
	require.Equal(t, []int{5, 3}, linkNs(b), "B builds on D (written under B) then C (written earlier, a back-reference), in B's refs order")
	c := nodeByTitle(t, nodes, "C")
	require.Equal(t, []int{1}, linkNs(c), "the cycle C→R is a back-reference to F01, never a second copy")
	require.NotEqual(t, nodes[0].Commit, rCitedByC(t, ri), "C cites an OLDER version of R than the one written")
	require.False(t, c.BuildsOn[0].DifferentVersion, "C cites an older R whose body is identical: not a different version")
	require.Equal(t, materialize.Truncation{}, first.Truncated, "nothing was cut at depth 2")
}

// rCitedByC is the commit C's edge pins R at.
func rCitedByC(t *testing.T, ri *repos.RepoInstance) string {
	t.Helper()
	var pinned string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		head, err := svc.HeadCommit(context.Background(), explainTestBranch)
		require.NoError(t, err)
		out, err := svc.GraphStore().OutgoingAtCommit(context.Background(), explainTestBranch, "kb/t1/c.md", head)
		require.NoError(t, err)
		require.Len(t, out, 1)
		pinned = out[0].Commit
	}))
	return pinned
}

// T2: depth is the SHORTEST distance from the root. R→[A, X], A→[X], X→[Y]
// at depth 2: the DFS reaches X first through A (stack depth 2), but X is one
// hop from R, so Y (two hops) is included.
func TestMaterialize_DepthIsShortestDistance(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	obs(t, ri, "kb/t2/y.md", "Y")
	obs(t, ri, "kb/t2/x.md", "X", "kb/t2/y.md")
	obs(t, ri, "kb/t2/a.md", "A", "kb/t2/x.md")
	obs(t, ri, "kb/t2/r.md", "R", "kb/t2/a.md", "kb/t2/x.md")

	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb/t2/r.md", "depth": 2})
	require.Equal(t, []string{"R", "A", "X", "Y"}, titles(nodes))
	require.Equal(t, 1, nodeByTitle(t, nodes, "X").Depth, "X's depth is its shortest distance")
	require.Equal(t, materialize.Truncation{}, first.Truncated)
}

// T7: the depth cap, the fact cap, and the defaults.
func TestMaterialize_Caps(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedT1(t, ri)

	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "depth": 1})
	require.Equal(t, []string{"R", "A", "B"}, titles(nodes))
	require.Equal(t, []string{"C|depth"}, omitted(nodeByTitle(t, nodes, "A")))
	require.Equal(t, []string{"D|depth", "C|depth"}, omitted(nodeByTitle(t, nodes, "B")))
	require.Equal(t, materialize.Truncation{Depth: 2}, first.Truncated, "two distinct facts cut by depth")

	nodes, first = humanFacts(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "depth": 2, "max_nodes": 3})
	require.Equal(t, []string{"R", "A", "B"}, titles(nodes), "BFS order R, A, B, C, D cut at 3")
	require.Equal(t, []string{"C|size"}, omitted(nodeByTitle(t, nodes, "A")))
	require.Equal(t, []string{"D|size", "C|size"}, omitted(nodeByTitle(t, nodes, "B")))
	require.Equal(t, materialize.Truncation{Nodes: 2}, first.Truncated)

	_, first = humanFacts(t, ctx, nil, map[string]any{"file": "kb/t1/r.md"})
	require.Equal(t, 4, first.Depth, "default depth")
	require.Equal(t, 200, first.MaxNodes, "default max_nodes")

	for _, bad := range []map[string]any{
		{"depth": 11}, {"depth": -1}, {"max_nodes": 0}, {"max_nodes": 501}, {"depth": 2.5},
	} {
		args := map[string]any{"file": "kb/t1/r.md", "audience": "agent"}
		for k, v := range bad {
			args[k] = v
		}
		res, text := matCallRaw(t, ctx, nil, args)
		require.True(t, res.IsError, "%v must be refused, got %s", bad, text)
	}
}

// versionsFixture: X cites A@c1 (A1 cites F); then A is edited to A2 (cites
// E instead); S cites A@c2; R cites [X, S]. xCommit is an anchor at which A
// is still A1.
type versionsFixture struct{ xCommit, c1, c2 string }

func seedVersions(t *testing.T, ri *repos.RepoInstance) versionsFixture {
	var v versionsFixture
	obs(t, ri, "kb/v/f.md", "F")
	obs(t, ri, "kb/v/e.md", "E")
	v.c1 = writeMat(t, ri, "kb/v/a.md", "A", "A first body.", fact.Observation, "kb/v/f.md")
	v.xCommit = obs(t, ri, "kb/v/x.md", "X", "kb/v/a.md")
	v.c2 = writeMat(t, ri, "kb/v/a.md", "A", "A second body.", fact.Observation, "kb/v/e.md")
	obs(t, ri, "kb/v/s.md", "S", "kb/v/a.md")
	obs(t, ri, "kb/v/r.md", "R", "kb/v/x.md", "kb/v/s.md")
	return v
}

func agentText(t *testing.T, ctx context.Context, mgr *repos.Manager, args map[string]any) string {
	t.Helper()
	args["audience"] = "agent"
	var sb strings.Builder
	for _, p := range matAll(t, ctx, mgr, args) {
		require.NotNil(t, p.Text)
		sb.WriteString(*p.Text)
	}
	return sb.String()
}

// T3/T4 + ruling 9: pinned mode reads the CITED version; a path is written
// once, at the version it was first reached at; a later citation of a version
// whose BODY differs is marked; a retraction after citing changes nothing.
func TestMaterialize_PinnedVersionsAndRetraction(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	v := seedVersions(t, ri)

	nodes, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/v/r.md"})
	require.Equal(t, []string{"R", "X", "A", "F", "S"}, titles(nodes), "A is read at X's cited version (A1, which cites F); E (A2 only) never appears")
	a := nodeByTitle(t, nodes, "A")
	require.Equal(t, v.c1, a.Commit)
	require.Equal(t, "A first body.", a.Body)
	s := nodeByTitle(t, nodes, "S")
	require.Len(t, s.BuildsOn, 1)
	require.Equal(t, 3, s.BuildsOn[0].N)
	require.True(t, s.BuildsOn[0].DifferentVersion, "S cites A2, whose body differs from the A1 written")

	head := matCall(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "audience": "agent"}).Anchor
	before := agentText(t, ctx, nil, map[string]any{"file": "kb/v/r.md"})
	require.Contains(t, before, "A first body.")
	require.Contains(t, before, "(cited here at a different version)")

	retractMat(t, ri, "kb/v/a.md")
	after := agentText(t, ctx, nil, map[string]any{"file": "kb/v/r.md"})
	newHead := matCall(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "audience": "agent"}).Anchor
	require.NotEqual(t, head, newHead)
	require.Equal(t, strings.Replace(before, head[:7], "ANCHOR", 1), strings.Replace(after, newHead[:7], "ANCHOR", 1),
		"retracting A after it was cited changes nothing but the anchor in pinned mode")
}

// T5: unpin reads every ref at its latest version; a gone target is not
// rendered and not mentioned, and what was reachable only through it drops
// out too.
func TestMaterialize_Unpin(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	v := seedVersions(t, ri)

	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "unpin": true})
	require.True(t, first.Unpinned)
	require.Equal(t, []string{"R", "X", "A", "E", "S"}, titles(nodes), "A at its latest version (A2, which cites E, not F)")
	a := nodeByTitle(t, nodes, "A")
	require.Equal(t, v.c2, a.Commit)
	require.Equal(t, "A second body.", a.Body)
	require.False(t, nodeByTitle(t, nodes, "S").BuildsOn[0].DifferentVersion, "under unpin every path has one version")

	// An explicit anchor: latest AS OF that commit.
	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/v/x.md", "unpin": true, "commit": v.xCommit})
	require.Equal(t, []string{"X", "A", "F"}, titles(nodes))
	require.Equal(t, "A first body.", nodeByTitle(t, nodes, "A").Body)

	retractMat(t, ri, "kb/v/a.md")
	nodes, first = humanFacts(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "unpin": true})
	require.Equal(t, []string{"R", "X", "S"}, titles(nodes), "A is gone, and E was reachable only through it")
	require.Equal(t, []int{2, 3}, linkNs(nodes[0]))
	for _, n := range nodes {
		require.Empty(t, n.NotIncluded, "%s: a gone target is never mentioned", n.Title)
	}
	require.Empty(t, nodeByTitle(t, nodes, "X").BuildsOn)
	require.Equal(t, materialize.Truncation{}, first.Truncated, "a gone target is never counted")
	text := agentText(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "unpin": true})
	require.NotContains(t, text, "kb/v/a.md")
	require.NotContains(t, text, "`a`")
}

// T6: under unpin the resolved versions are frozen in the cursor snapshot: a
// commit landing between pages changes nothing in later pages; a new call
// sees it.
func TestMaterialize_UnpinCursorIsFrozen(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedVersions(t, ri)

	p1 := matCall(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "audience": "human", "unpin": true, "limit": 1})
	require.Equal(t, []string{"R"}, titles(p1.Facts))
	require.True(t, p1.HasMore)

	writeMat(t, ri, "kb/v/a.md", "A", "A third body.", fact.Observation, "kb/v/e.md")

	var rest []materialize.Node
	p := p1
	for p.HasMore {
		p = matCall(t, ctx, nil, map[string]any{"cursor": *p.Cursor, "limit": 1})
		rest = append(rest, p.Facts...)
	}
	require.Equal(t, []string{"X", "A", "E", "S"}, titles(rest))
	require.Equal(t, "A second body.", nodeByTitle(t, rest, "A").Body, "later pages read the snapshot's version")

	fresh, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/v/r.md", "unpin": true})
	require.Equal(t, "A third body.", nodeByTitle(t, fresh, "A").Body, "a new call sees the new version")
}

// T8: every non-fact ref lands in the right list with the right reason, and
// none becomes a node.
func TestMaterialize_RefKinds(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	obs(t, ri, "kb/k/r.md", "R",
		"kb://ffffffffffff/kb/x/y.md",
		".knomit/ontology.yaml",
		"src://7b4887ce51d9/internal/edge/mcp/query.go@0610c20083fcef9e84f42ab0311a14708bcb6193:ca03e74bb60c2ee60d9ea5fdde1fec35f0b8a102",
		"https://example.com/blog/a-post.html",
		"https://github.com/knomit/knomit/pull/1",
	)
	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb/k/r.md"})
	require.Equal(t, 1, first.Total, "no ref here is a fact this walk can read")
	r := nodes[0]
	require.Equal(t, []string{"kb://ffffffffffff/kb/x/y.md|cross_kb_off", ".knomit/ontology.yaml|system_file"}, omitted(r))
	require.Equal(t, []materialize.Source{
		{Label: "query.go (code @0610c20)"},
		{Label: "example.com: a-post", URL: "https://example.com/blog/a-post.html"},
		{Label: "github.com/knomit/knomit", URL: "https://github.com/knomit/knomit/pull/1"},
	}, r.Sources)
}

// T9: cross-KB refs, followed only on request, into ANY registered repo (here
// one the caller's binding does not mount), at that repo's LATEST version.
func TestMaterialize_CrossKB(t *testing.T) {
	m, alpha, beta, _, _, _ := newLensE2E(t)
	// The binding mounts alpha alone; the caller is a local one (every edge
	// records a reach, and a local caller's is every repo).
	ctx := auth.WithRepoReach(repos.WithRepoInstance(context.Background(), alpha), auth.AllRepos)
	beta12 := fact.ID12(beta.ID())
	xWire := "kb://" + beta12 + "/kb/b/x.md"
	zWire := "kb://0123456789ab/kb/z.md"

	obs(t, beta, "kb/b/y.md", "Y")
	writeMat(t, beta, "kb/b/x.md", "X", "X first body.", fact.Observation, "kb/b/y.md")
	// N5: the same target spelled twice. The uppercase id and path classify
	// to the same key as xWire, so they are ONE ref. (A full 40-hex id cannot
	// be stored at all: kb:// wants exactly 12 hex, and SerializeFact refuses
	// any other id.)
	obs(t, alpha, "kb/a/r.md", "R", xWire, "kb://"+strings.ToUpper(beta12)+"/kb/B/X.md", zWire)
	writeMat(t, beta, "kb/b/x.md", "X", "X second body.", fact.Observation, "kb/b/y.md") // after R cited it

	nodes, _ := humanFacts(t, ctx, m, map[string]any{"file": "kb/a/r.md"})
	require.Equal(t, []string{"R"}, titles(nodes))
	require.Equal(t, []string{xWire + "|cross_kb_off", zWire + "|cross_kb_off"}, omitted(nodes[0]))

	nodes, first := humanFacts(t, ctx, m, map[string]any{"file": "kb/a/r.md", "cross_kb": true})
	require.True(t, first.CrossKB)
	require.Equal(t, []string{"R", "X", "Y"}, titles(nodes))
	require.Equal(t, []int{2}, linkNs(nodes[0]), "one Builds-on link to X, however the ref was spelled")
	x := nodeByTitle(t, nodes, "X")
	require.Equal(t, xWire, x.Path)
	require.Equal(t, "X second body.", x.Body, "a cross-KB ref has no cited version: the LATEST is read")
	require.Equal(t, []string{materialize.MarkLatestOtherKB}, x.Marks)
	y := nodeByTitle(t, nodes, "Y")
	require.Equal(t, "kb://"+beta12+"/kb/b/y.md", y.Path)
	require.Empty(t, y.Marks, "inside the other repo, refs are pinned by its own edges")
	require.Equal(t, []string{zWire + "|kb_unavailable"}, omitted(nodes[0]))

	// A caller whose credential does not reach beta (a bearer token confined
	// to alpha) gets the SAME answer as for a repo that does not exist.
	confined := auth.WithRepoReach(ctx, func(repo string) bool { return repo == "alpha" })
	nodes, _ = humanFacts(t, confined, m, map[string]any{"file": "kb/a/r.md", "cross_kb": true})
	require.Equal(t, []string{"R"}, titles(nodes))
	require.Equal(t, []string{xWire + "|kb_unavailable", zWire + "|kb_unavailable"}, omitted(nodes[0]))

	// A context with NO recorded reach reaches nothing beyond the binding:
	// ReachesRepo is default-deny.
	bare := repos.WithRepoInstance(context.Background(), alpha)
	nodes, _ = humanFacts(t, bare, m, map[string]any{"file": "kb/a/r.md", "cross_kb": true})
	require.Equal(t, []string{"R"}, titles(nodes))
	require.Equal(t, []string{xWire + "|kb_unavailable", zWire + "|kb_unavailable"}, omitted(nodes[0]))

	// Without the Manager only the binding's own mounts are reachable.
	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/a/r.md", "cross_kb": true})
	require.Equal(t, []string{"R"}, titles(nodes))
	require.Equal(t, []string{xWire + "|kb_unavailable", zWire + "|kb_unavailable"}, omitted(nodes[0]))

	retractMat(t, beta, "kb/b/x.md")
	nodes, _ = humanFacts(t, ctx, m, map[string]any{"file": "kb/a/r.md", "cross_kb": true})
	require.Equal(t, []string{"R"}, titles(nodes))
	require.Equal(t, []string{xWire + "|not_in_that_kb", zWire + "|kb_unavailable"}, omitted(nodes[0]))

	nodes, _ = humanFacts(t, ctx, m, map[string]any{"file": "kb/a/r.md", "cross_kb": true, "unpin": true})
	require.Equal(t, []string{"R"}, titles(nodes))
	require.Equal(t, []string{zWire + "|kb_unavailable"}, omitted(nodes[0]), "under unpin a gone cross-KB target is not mentioned")
}

// T10: a frame's ancestors come first, topologically (each before any frame
// citing it, a shared one once, ties by refs order), then the frame, then the
// ordinary walk. An ancestor is written with its own content only: none of
// its other refs is expanded or listed.
func TestMaterialize_FrameAncestors(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	obs(t, ri, "kb/f/n1.md", "N1")
	frame(t, ri, "kb/frames/t.md", "T")
	frame(t, ri, "kb/frames/p1.md", "P1", "kb/frames/t.md", "kb/f/n1.md", "kb://ffffffffffff/kb/q.md") // N1 and a cross-KB ref: neither expanded nor listed
	frame(t, ri, "kb/frames/p2.md", "P2", "kb/frames/t.md")
	frame(t, ri, "kb/frames/sib.md", "Sib", "kb/frames/p1.md") // a child of P1: never appears
	obs(t, ri, "kb/f/g.md", "G")
	obs(t, ri, "kb/f/k.md", "K", "kb/f/g.md")
	frame(t, ri, "kb/frames/f.md", "F", "kb/frames/p2.md", "kb/frames/p1.md", "kb/f/k.md")

	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/f.md"})
	require.Equal(t, "frame", first.Order)
	require.Equal(t, []string{"T", "P2", "P1", "F", "K", "G"}, titles(nodes),
		"T (shared by P2 and P1) once, first; P2 before P1 by F's refs order; never N1 (P1's non-frame ref) nor Sib (a child)")
	roles := make([]string, len(nodes))
	sections := make([]string, len(nodes))
	for i, n := range nodes {
		roles[i], sections[i] = n.Role, n.Section
	}
	require.Equal(t, []string{"ancestor", "ancestor", "ancestor", "focal", "ref", "ref"}, roles)
	require.Equal(t, []string{"Ancestors", "", "", "This frame", "What it stands on", ""}, sections)
	p1 := nodeByTitle(t, nodes, "P1")
	require.Equal(t, []int{1}, linkNs(p1), "P1 links its parent T")
	require.Empty(t, p1.NotIncluded, "an ancestor's other refs are not listed either")
	require.Equal(t, []int{2, 3}, linkNs(nodeByTitle(t, nodes, "F"))[:2], "F's parents are back-references")

	// The user's example: frame A has frame children B and C; materializing
	// B gives "A | B", never C.
	frame(t, ri, "kb/frames/ua.md", "UA")
	frame(t, ri, "kb/frames/ub.md", "UB", "kb/frames/ua.md")
	frame(t, ri, "kb/frames/uc.md", "UC", "kb/frames/ua.md")
	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/ub.md"})
	require.Equal(t, []string{"UA", "UB"}, titles(nodes))
}

// T11: ancestors are included whatever their distance: depth does not bound
// them, only max_nodes does, closest first.
func TestMaterialize_FrameAncestorsBeyondDepth(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	frame(t, ri, "kb/frames/c1.md", "C1")
	for _, p := range [][2]string{{"c2", "c1"}, {"c3", "c2"}, {"c4", "c3"}, {"c5", "c4"}, {"c6", "c5"}} {
		frame(t, ri, "kb/frames/"+p[0]+".md", strings.ToUpper(p[0]), "kb/frames/"+p[1]+".md")
	}
	nodes, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/c6.md", "depth": 0})
	require.Equal(t, []string{"C1", "C2", "C3", "C4", "C5", "C6"}, titles(nodes))
	require.Equal(t, 5, nodes[0].Depth, "an ancestor's depth is its upward hop count")

	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/c6.md", "max_nodes": 3})
	require.Equal(t, []string{"C4", "C5", "C6"}, titles(nodes), "the cap keeps the CLOSEST ancestors")
}

// Several parents, a frame cycle, and frame ancestry under unpin (latest
// versions: a parent removed from refs or retracted drops out with what was
// reachable only through it).
func TestMaterialize_FrameUnpinAndCycle(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	frame(t, ri, "kb/frames/t.md", "T")
	frame(t, ri, "kb/frames/q.md", "Q")
	frame(t, ri, "kb/frames/p1.md", "P1", "kb/frames/t.md")
	frame(t, ri, "kb/frames/p2.md", "P2", "kb/frames/q.md")
	frame(t, ri, "kb/frames/f.md", "F", "kb/frames/p1.md", "kb/frames/p2.md")

	nodes, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/f.md"})
	require.Equal(t, []string{"T", "P1", "Q", "P2", "F"}, titles(nodes), "two parents, each line topological, in F's refs order")

	frame(t, ri, "kb/frames/p1.md", "P1") // P1 drops its ref to T
	retractMat(t, ri, "kb/frames/p2.md")

	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/f.md"})
	require.Equal(t, []string{"T", "P1", "Q", "P2", "F"}, titles(nodes), "pinned: the cited versions, unchanged")

	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/f.md", "unpin": true})
	require.Equal(t, []string{"P1", "F"}, titles(nodes), "unpin: T left P1's refs, P2 is retracted and Q was reachable only through it")
	require.Empty(t, nodeByTitle(t, nodes, "F").NotIncluded)

	// Two frames that cite each other above the start.
	frame(t, ri, "kb/frames/g1.md", "G1")
	frame(t, ri, "kb/frames/g2.md", "G2", "kb/frames/g1.md")
	frame(t, ri, "kb/frames/g1.md", "G1", "kb/frames/g2.md")
	frame(t, ri, "kb/frames/g2.md", "G2", "kb/frames/g1.md")
	frame(t, ri, "kb/frames/h.md", "H", "kb/frames/g1.md")
	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/h.md"})
	require.Equal(t, []string{"G2", "G1", "H"}, titles(nodes), "G1 (reached first from H) is the child, G2 its parent; G2's ref back to G1 is a back-reference")
}

// T15: paging. Contiguous ordinals, constant total, instructions on page 1
// only, and a cursor refused through a foreign binding, with document
// arguments, or when minted by another tool.
func TestMaterialize_Paging(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedT1(t, ri)

	pages := matAll(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "audience": "human", "limit": 2})
	require.Len(t, pages, 3)
	next := 1
	for i, p := range pages {
		require.Equal(t, 5, p.Total)
		require.Equal(t, next, p.NFrom, "page %d", i)
		require.Equal(t, p.NTo-p.NFrom+1, len(p.Facts))
		for j, f := range p.Facts {
			require.Equal(t, p.NFrom+j, f.N)
			require.NotEmpty(t, f.Body, "every human fact carries its body")
		}
		next = p.NTo + 1
		require.Equal(t, materialize.InstructionsVersion, p.InstructionsVersion)
		if i == 0 {
			require.Equal(t, materialize.HumanInstructions, p.Instructions)
		} else {
			require.Empty(t, p.Instructions)
		}
	}
	require.False(t, pages[2].HasMore)
	require.Nil(t, pages[2].Cursor)

	p1 := matCall(t, ctx, nil, map[string]any{"file": "kb/t1/r.md", "audience": "agent", "limit": 1})
	require.True(t, p1.HasMore)

	res, text := matCallRaw(t, ctx, nil, map[string]any{"cursor": *p1.Cursor, "depth": 1})
	require.True(t, res.IsError)
	require.Contains(t, text, "depth cannot be passed with cursor")

	other := newLearnTestRepo(t, fact.CodeOntology())
	res, text = matCallRaw(t, repos.WithRepoInstance(context.Background(), other), nil, map[string]any{"cursor": *p1.Cursor})
	require.True(t, res.IsError)
	require.Equal(t, errMaterializeExpired, text, "a cursor through another binding reads as expired")

	// A knomit_explain cursor is not a materialize cursor.
	var ereq mcpgo.CallToolRequest
	ereq.Params.Arguments = map[string]any{"file": "kb/t1/r.md"}
	eres, err := ExplainHandler()(ctx, ereq)
	require.NoError(t, err)
	var er expResp
	require.NoError(t, json.Unmarshal([]byte(resultText(t, eres)), &er))
	require.NotNil(t, er.Cursor)
	res, text = matCallRaw(t, ctx, nil, map[string]any{"cursor": *er.Cursor})
	require.True(t, res.IsError)
	require.Equal(t, errMaterializeExpired, text)
}

// The page cap is measured: facts whose bodies together exceed it are split
// across pages even under a large limit, and a fact larger than the cap
// alone is still delivered.
func TestMaterialize_PagesStayUnderTheCap(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	big := strings.Repeat("word ", 2400) // 12 KB each
	writeMat(t, ri, "kb/big/c.md", "C", big, fact.Observation)
	writeMat(t, ri, "kb/big/b.md", "B", big, fact.Observation)
	writeMat(t, ri, "kb/big/a.md", "A", big, fact.Observation)
	writeMat(t, ri, "kb/big/huge.md", "Huge", strings.Repeat("word ", 9000), fact.Observation)
	writeMat(t, ri, "kb/big/r.md", "R", "root", fact.Observation, "kb/big/a.md", "kb/big/b.md", "kb/big/c.md", "kb/big/huge.md")

	for _, audience := range []string{"agent", "human"} {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{"file": "kb/big/r.md", "audience": audience, "limit": 5}
		var sizes, counts []int
		for {
			res, err := MaterializeHandler(nil)(ctx, req)
			require.NoError(t, err)
			text := resultText(t, res)
			require.False(t, res.IsError, text)
			var p matPage
			require.NoError(t, json.Unmarshal([]byte(text), &p))
			sizes = append(sizes, len(text))
			counts = append(counts, p.NTo-p.NFrom+1)
			if !p.HasMore {
				break
			}
			req.Params.Arguments = map[string]any{"cursor": *p.Cursor, "limit": 5}
		}
		require.Equal(t, []int{3, 1, 1}, counts, "%s: R+A+B fit under the cap, C does not join them; Huge goes alone", audience)
		for i, n := range sizes[:2] {
			require.LessOrEqual(t, n, materializePageCap, "%s page %d", audience, i+1)
		}
		require.Greater(t, sizes[2], materializePageCap, "%s: the oversized fact is still delivered, alone", audience)
	}
}

// T14 (MCP half): the agent pages concatenate, byte for byte, to the one
// document RenderAgent produces from the same plan, which is what the REST
// route returns (its half is in internal/edge/web/api/kb).
func TestMaterialize_AgentPagesConcatenateToTheDocument(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	seedT1(t, ri)
	obs(t, ri, "kb/t1/src.md", "Src", "kb/t1/r.md", "https://example.com/x/y-z", "src://7b4887ce51d9/a/b.go@0610c20083fcef9e84f42ab0311a14708bcb6193:ca03e74bb60c2ee60d9ea5fdde1fec35f0b8a102")

	b, err := repos.RequireBinding(ctx)
	require.NoError(t, err)
	doc, err := materialize.Plan(ctx, materialize.Request{Binding: b, File: "kb/t1/src.md", Options: materialize.Options{Depth: 2, MaxNodes: 3}})
	require.NoError(t, err)
	want := materialize.RenderAgent(doc)

	for _, limit := range []int{1, 2, 100} {
		got := agentText(t, ctx, nil, map[string]any{"file": "kb/t1/src.md", "depth": 2, "max_nodes": 3, "limit": limit})
		require.Equal(t, want, got, "limit %d", limit)
	}

	// The shape, with the volatile parts (anchor, dates) normalized.
	norm := strings.ReplaceAll(want, doc.Anchor[:7], "ANCHOR")
	for _, n := range doc.Nodes {
		require.NotEmpty(t, n.Date)
		norm = strings.ReplaceAll(norm, n.Date, "DATE")
	}
	bt := "`"
	require.Equal(t, `# Src

Materialized from `+bt+`kb/t1/src.md`+bt+` at commit ANCHOR.
3 knowledge-base facts, each reproduced in full and unedited, in reading order.
Each fact starts with: id, date of the version shown, confidence label for epistemic facts (well-established = 0.85 or above, likely = 0.65 to 0.84, tentative = below 0.65), and type.
"Builds on" links point to another fact in this document. "Related, not included" lines are titles only, with the reason they were not expanded.
Caveat lines inside a fact ("does not mean", "not verified", scope notes) are part of its claim; keep them when quoting.
Not expanded: 1 cited fact(s) beyond the depth limit (2), 1 cut by the size limit (3).

<a id="f-src"></a>
### Src
`+bt+`src`+bt+` · DATE · well-established · observation

Src body.

Builds on: [R](#f-r) (`+bt+`r`+bt+`)

Sources: [example.com: y-z](https://example.com/x/y-z), b.go (code @0610c20)

<a id="f-r"></a>
### R
`+bt+`r`+bt+` · DATE · well-established · observation

R body.

Builds on: [A](#f-a) (`+bt+`a`+bt+`)

Related, not included: B (`+bt+`b`+bt+`, size limit)

<a id="f-a"></a>
### A
`+bt+`a`+bt+` · DATE · well-established · observation

A body.

Related, not included: C (`+bt+`c`+bt+`, depth limit)
`, norm)
}

// T16: a root addressed in a read mount of a lens: every node of that mount
// is qualified with it.
func TestMaterialize_LensReadMountRoot(t *testing.T) {
	riA := newLearnTestRepo(t, fact.CodeOntology())
	riB := newLearnTestRepo(t, fact.CodeOntology())
	obs(t, riB, "kb/m/y.md", "Y")
	obs(t, riB, "kb/m/x.md", "X", "kb/m/y.md")
	require.NotEqual(t, riA.ID(), riB.ID())
	b := repos.NewBindingForTest(riA,
		repos.ReadTarget{RI: riA, Branch: explainTestBranch},
		repos.ReadTarget{RI: riB, Branch: explainTestBranch},
	)
	ctx := repos.WithBinding(context.Background(), b)
	b12 := fact.ID12(riB.ID())

	nodes, first := humanFacts(t, ctx, nil, map[string]any{"file": "kb://" + b12 + "/kb/m/x.md"})
	require.Equal(t, "kb://"+b12+"/kb/m/x.md", first.Root)
	require.Equal(t, []string{"kb://" + b12 + "/kb/m/x.md", "kb://" + b12 + "/kb/m/y.md"}, []string{nodes[0].Path, nodes[1].Path})
	require.Empty(t, nodes[1].Marks, "the root's own mount is not a cross-KB hop")
}

// B1: a cursor is a frozen view of ONE binding, and the binding pin is not
// per principal: a whole-API caller and an alpha-confined caller on alpha's
// endpoint share it. Resuming must re-check the RESUMING caller's reach, so a
// confined caller cannot page through beta nodes a wider caller snapshotted.
func TestMaterialize_ResumeRechecksTheCallersReach(t *testing.T) {
	m, alpha, beta, _, _, _ := newLensE2E(t)
	base := repos.WithRepoInstance(context.Background(), alpha)
	wide := auth.WithRepoReach(base, auth.AllRepos)
	confined := auth.WithRepoReach(base, func(repo string) bool { return repo == "alpha" })
	xWire := "kb://" + fact.ID12(beta.ID()) + "/kb/b/x.md"
	obs(t, beta, "kb/b/x.md", "X")
	obs(t, alpha, "kb/a/r.md", "R", xWire)

	p1 := matCall(t, wide, m, map[string]any{"file": "kb/a/r.md", "audience": "human", "cross_kb": true, "limit": 1})
	require.Equal(t, []string{"R"}, titles(p1.Facts))
	require.True(t, p1.HasMore, "X, from beta, is on page 2")

	res, text := matCallRaw(t, confined, m, map[string]any{"cursor": *p1.Cursor, "limit": 1})
	require.True(t, res.IsError, "a caller that does not reach beta must not read X through another caller's cursor: %s", text)
	require.True(t, strings.HasPrefix(text, errMaterializeExpired), text)
	require.NotContains(t, text, "X body.")

	p2 := matCall(t, wide, m, map[string]any{"cursor": *p1.Cursor, "limit": 1})
	require.Equal(t, []string{"X"}, titles(p2.Facts), "the refused page was not consumed")
	require.Equal(t, "X body.", p2.Facts[0].Body)
}

// writeFramesOneCommit writes several frames in ONE commit (they may cite
// each other: every ref is pinned at that commit).
func writeFramesOneCommit(t *testing.T, ri *repos.RepoInstance, frames map[string][]string) string {
	t.Helper()
	files := map[string]string{}
	for path, refs := range frames {
		f := fact.NewFact(path)
		f.Title = strings.ToUpper(strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".md"))
		f.Body = f.Title + " frame body."
		f.Type, f.Kind = fact.Frame, fact.Pragmatic
		f.Domain, f.Entities, f.Refs = []string{"testing"}, []string{}, refs
		f.Confidence, f.Sources = 0.9, 1
		content, err := fact.SerializeFact(f)
		require.NoError(t, err)
		files[path] = content
	}
	var commit string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		c, _, err := svc.Facts().BatchWriteFacts(context.Background(), explainTestBranch, files, nil, "write frames", "")
		require.NoError(t, err)
		commit = c
	}))
	return commit
}

// Round-3 ruling: the frame you are at is the child and every frame it refs
// is a parent. Two frames at the SAME commit that ref each other: the
// starting one is the child, the other its parent, whichever you start from.
func TestMaterialize_SameCommitMutualFramesTheStartIsTheChild(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	writeFramesOneCommit(t, ri, map[string][]string{
		"kb/frames/p.md": {"kb/frames/q.md"},
		"kb/frames/q.md": {"kb/frames/p.md"},
	})

	nodes, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/q.md"})
	require.Equal(t, []string{"P", "Q"}, titles(nodes))
	require.Equal(t, []string{"ancestor", "focal"}, []string{nodes[0].Role, nodes[1].Role})
	require.Equal(t, []int{2}, linkNs(nodes[0]), "P's ref back to Q is a back-reference, not a second copy")

	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/p.md"})
	require.Equal(t, []string{"Q", "P"}, titles(nodes), "started from P, P is the child")
	require.Equal(t, []string{"ancestor", "focal"}, []string{nodes[0].Role, nodes[1].Role})

	// The same pair ABOVE the start: S→P, P↔Q. P is reached first, so P is
	// Q's child and Q comes first; the pair never deadlocks the order.
	frame(t, ri, "kb/frames/s.md", "S", "kb/frames/p.md")
	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/s.md"})
	require.Equal(t, []string{"Q", "P", "S"}, titles(nodes))
}

// Review N2's shape, F→X, X→Y, Y↔Z: the ancestor order never lifts a child
// above its parent. Y is reached before Z, so Y is the child of Z and Z's ref
// back to Y is a back-reference.
func TestMaterialize_FrameCycleBelowDoesNotLiftAChild(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	frame(t, ri, "kb/frames/y.md", "Y")
	frame(t, ri, "kb/frames/z.md", "Z", "kb/frames/y.md")
	writeMat(t, ri, "kb/frames/y.md", "Y", "Y now cites Z.", fact.Frame, "kb/frames/z.md")
	frame(t, ri, "kb/frames/x.md", "X", "kb/frames/y.md")
	frame(t, ri, "kb/frames/f.md", "F", "kb/frames/x.md")

	nodes, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/f.md", "depth": 0})
	require.Equal(t, []string{"Z", "Y", "X", "F"}, titles(nodes), "each ancestor before the frame citing it")
}

// N10: a frame retracted and learned again at the same path is read, through
// a ref pinned before that, at the PINNED version; unpin reads the new one.
func TestMaterialize_RelearnedFrameReadAtItsPinnedVersion(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)
	v1 := writeMat(t, ri, "kb/frames/a.md", "A", "A as first learned.", fact.Frame)
	frame(t, ri, "kb/frames/b.md", "B", "kb/frames/a.md")
	retractMat(t, ri, "kb/frames/a.md")
	v2 := writeMat(t, ri, "kb/frames/a.md", "A", "A learned again.", fact.Frame)

	nodes, _ := humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/b.md"})
	require.Equal(t, []string{"A", "B"}, titles(nodes))
	require.Equal(t, v1, nodes[0].Commit)
	require.Equal(t, "A as first learned.", nodes[0].Body)

	nodes, _ = humanFacts(t, ctx, nil, map[string]any{"file": "kb/frames/b.md", "unpin": true})
	require.Equal(t, v2, nodes[0].Commit)
	require.Equal(t, "A learned again.", nodes[0].Body)
}

// A frame's parent in ANOTHER repo is a cross-KB ref like any other: followed
// with cross_kb and the caller's reach (latest version there, and the
// ancestor walk continues in that repo under the same rules); "not included"
// with cross_kb off; kb_unavailable for a caller that does not reach it.
func TestMaterialize_CrossRepoFrameParent(t *testing.T) {
	m, alpha, beta, _, _, _ := newLensE2E(t)
	base := repos.WithRepoInstance(context.Background(), alpha)
	wide := auth.WithRepoReach(base, auth.AllRepos)
	beta12 := fact.ID12(beta.ID())
	pWire := "kb://" + beta12 + "/kb/frames/p.md"
	frame(t, beta, "kb/frames/t.md", "T")
	frame(t, beta, "kb/frames/p.md", "P", "kb/frames/t.md")
	frame(t, alpha, "kb/frames/f.md", "F", pWire)

	nodes, _ := humanFacts(t, wide, m, map[string]any{"file": "kb/frames/f.md", "cross_kb": true})
	require.Equal(t, []string{"T", "P", "F"}, titles(nodes), "the walk continues inside beta")
	require.Equal(t, []string{"kb://" + beta12 + "/kb/frames/t.md", pWire, "kb/frames/f.md"}, []string{nodes[0].Path, nodes[1].Path, nodes[2].Path})
	require.Equal(t, []string{"ancestor", "ancestor", "focal"}, []string{nodes[0].Role, nodes[1].Role, nodes[2].Role})
	require.Equal(t, []string{materialize.MarkLatestOtherKB}, nodes[1].Marks)

	nodes, _ = humanFacts(t, wide, m, map[string]any{"file": "kb/frames/f.md"})
	require.Equal(t, []string{"F"}, titles(nodes))
	require.Equal(t, []string{pWire + "|cross_kb_off"}, omitted(nodes[0]))

	nodes, _ = humanFacts(t, base, m, map[string]any{"file": "kb/frames/f.md", "cross_kb": true})
	require.Equal(t, []string{"F"}, titles(nodes), "no reach: refused")
	require.Equal(t, []string{pWire + "|kb_unavailable"}, omitted(nodes[0]))
}
