package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// --- test-local mirrors of the explain JSON response shape ---

type expRev struct {
	Commit  string          `json:"commit"`
	Date    string          `json:"date"`
	Message string          `json:"message"`
	Diff    json.RawMessage `json:"diff"`
}

type expHistory struct {
	Revisions     []expRev `json:"revisions"`
	MoreAvailable bool     `json:"more_available"`
}

type expRefs struct {
	Local    []string `json:"local"`
	External []string `json:"external"`
}

type expFact struct {
	Path       string      `json:"path"`
	Commit     string      `json:"commit"`
	Depth      int         `json:"depth"`
	Title      string      `json:"title"`
	Type       string      `json:"type"`
	Kind       string      `json:"kind"`
	Confidence float64     `json:"confidence"`
	Body       string      `json:"body"`
	Summary    bool        `json:"summary"`
	Deleted    bool        `json:"deleted"`
	Refs       *expRefs    `json:"refs"`
	History    *expHistory `json:"history"`
}

type expResp struct {
	Facts   []expFact `json:"facts"`
	Cursor  *string   `json:"cursor"`
	HasMore bool      `json:"has_more"`
}

const explainTestBranch = "agent/test"

// writeExplainFact writes a fact at path via the store directly (bypassing
// ontology validation) and returns the commit hash it was written at.
func writeExplainFact(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path, title string, conf float64, refs []string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = title + " body text."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = conf
	f.Sources = 1
	f.Entities = []string{}
	f.Refs = refs
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)

	var commit string
	ri.WithRead(func(svc *store.Service) {
		r, werr := svc.Facts().WriteFact(ctx, explainTestBranch, path, content, "write "+path, "")
		require.NoError(t, werr)
		commit = r.CommitHash
	})
	require.NotEmpty(t, commit)
	return commit
}

// explainAll drives knomit_explain from the first call through every cursor
// page, accumulating all returned facts. commit is optional (anchor).
func explainAll(t *testing.T, ctx context.Context, file, commit string) []expFact {
	t.Helper()
	var all []expFact
	var req mcpgo.CallToolRequest
	args := map[string]any{"file": file}
	if commit != "" {
		args["commit"] = commit
	}
	req.Params.Arguments = args

	for {
		result, err := ExplainHandler()(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, result)
		text := resultText(t, result)
		require.False(t, result.IsError, "explain error: %s", text)

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
	return all
}

func findExpFact(facts []expFact, path string) *expFact {
	for i := range facts {
		if facts[i].Path == path {
			return &facts[i]
		}
	}
	return nil
}

// TestExplain_VersionedWalk_ReadsGrandchildAtParentsVersion regresses the bug
// where the walk propagated the root's commit to every node. The graph is
// versioned per-edge: a grandchild must be read at the version its PARENT
// pointed to, not at whatever version is active at the root's commit.
//
// Scenario (commit order matters):
//
//	cL1: write L v1
//	cM : write M referencing L   → edge M→L pinned at cL1
//	cL2: edit  L to v2           → L's active version is now cL2
//	cR : write R referencing M   → edge R→M pinned at cM
//
// explain(R) must surface L as "L-v1" (the version M pointed at), NOT "L-v2"
// (the version active at R's commit, which the buggy flat-snapshot walk returns).
func TestExplain_VersionedWalk_ReadsGrandchildAtParentsVersion(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/l.md", "L-v1", 0.91, nil)
	writeExplainFact(t, ctx, ri, "kb/m.md", "M", 0.80, []string{"kb/l.md"})
	writeExplainFact(t, ctx, ri, "kb/l.md", "L-v2", 0.71, nil)
	writeExplainFact(t, ctx, ri, "kb/r.md", "R", 0.70, []string{"kb/m.md"})

	facts := explainAll(t, ctx, "kb/r.md", "")

	l := findExpFact(facts, "kb/l.md")
	require.NotNil(t, l, "grandchild kb/l.md must appear in the walk; got %+v", facts)
	require.Equal(t, 2, l.Depth, "L is two hops from the root")
	require.Equal(t, "L-v1", l.Title,
		"grandchild must be read at M's pinned target_commit (cL1=L-v1), not root's commit (L-v2)")
}

// TestExplain_RootFullNodesAreSummaries pins the response asymmetry: the root
// carries body + history and is NOT a summary; every other node omits the body
// and is flagged summary:true while still carrying type/kind/confidence.
func TestExplain_RootFullNodesAreSummaries(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/child.md", "Child", 0.88, nil)
	writeExplainFact(t, ctx, ri, "kb/root.md", "Root", 0.90, []string{"kb/child.md"})

	facts := explainAll(t, ctx, "kb/root.md", "")

	root := findExpFact(facts, "kb/root.md")
	require.NotNil(t, root)
	require.Equal(t, 0, root.Depth)
	require.False(t, root.Summary, "root must not be a summary")
	require.NotEmpty(t, root.Body, "root must carry its body")
	require.NotNil(t, root.History, "root must carry history")

	child := findExpFact(facts, "kb/child.md")
	require.NotNil(t, child)
	require.True(t, child.Summary, "non-root nodes are summaries")
	require.Empty(t, child.Body, "summary nodes omit the body")
	require.Nil(t, child.History, "summary nodes omit history")
	require.Equal(t, "observation", child.Type, "summary keeps type")
	require.Equal(t, "epistemic", child.Kind, "summary keeps kind")
	require.InDelta(t, 0.88, child.Confidence, 1e-9, "summary keeps confidence")
}

// TestExplain_RootHistoryEvolution pins the root's evolution: revisions newest
// → oldest, confidence + body diffs vs predecessor, capped at 3 with
// more_available, and the creation revision carrying a null diff.
func TestExplain_RootHistoryEvolution(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	// Three versions → creation + two edits. No 4th, so more_available=false
	// and the oldest (creation) revision has a null diff.
	writeExplainFact(t, ctx, ri, "kb/h.md", "H one", 0.50, nil)
	writeExplainFact(t, ctx, ri, "kb/h.md", "H two words", 0.60, nil)
	c3 := writeExplainFact(t, ctx, ri, "kb/h.md", "H three more words", 0.80, nil)

	facts := explainAll(t, ctx, "kb/h.md", "")
	root := findExpFact(facts, "kb/h.md")
	require.NotNil(t, root)
	require.NotNil(t, root.History)
	require.Len(t, root.History.Revisions, 3, "all three revisions shown")
	require.False(t, root.History.MoreAvailable, "only three revisions exist")

	revs := root.History.Revisions
	require.Equal(t, c3, revs[0].Commit, "newest revision first")

	// Newest revision: confidence 0.6 → 0.8 and a body change.
	require.NotNil(t, revs[0].Diff)
	var d0 revisionDiff
	require.NoError(t, json.Unmarshal(revs[0].Diff, &d0))
	require.Equal(t, []float64{0.60, 0.80}, d0.Confidence)
	require.NotEmpty(t, d0.Body, "body changed between v2 and v3")

	// Oldest (creation) has no predecessor → null diff. json.RawMessage
	// captures JSON null as the literal bytes "null", not a Go nil slice.
	require.Equal(t, "null", string(revs[2].Diff),
		"the creation revision has no predecessor diff")
}

// TestExplain_HistoryCappedWithMoreAvailable pins that a 4th revision flips
// more_available true while still showing exactly 3 revisions (and the oldest
// shown gets a real diff, since its predecessor exists as the diff base).
func TestExplain_HistoryCappedWithMoreAvailable(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/c.md", "C v1", 0.50, nil)
	writeExplainFact(t, ctx, ri, "kb/c.md", "C v2", 0.55, nil)
	writeExplainFact(t, ctx, ri, "kb/c.md", "C v3", 0.60, nil)
	writeExplainFact(t, ctx, ri, "kb/c.md", "C v4", 0.65, nil)

	facts := explainAll(t, ctx, "kb/c.md", "")
	root := findExpFact(facts, "kb/c.md")
	require.NotNil(t, root)
	require.NotNil(t, root.History)
	require.Len(t, root.History.Revisions, 3, "capped at three")
	require.True(t, root.History.MoreAvailable, "a fourth revision exists")
	require.NotNil(t, root.History.Revisions[2].Diff, "oldest shown still diffs against its (unshown) predecessor")
}

// TestExplain_CommitAnchorRewindsGraph pins that anchoring at an older commit
// rewinds the graph: a child added after the anchor does not appear, and the
// root's history is bounded to the anchor's ancestry.
func TestExplain_CommitAnchorRewindsGraph(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/early.md", "Early", 0.80, nil)
	// Root v1 references early only.
	cRoot1 := writeExplainFact(t, ctx, ri, "kb/r.md", "Root v1", 0.70, []string{"kb/early.md"})
	// Later: a new fact, and root v2 references it too.
	writeExplainFact(t, ctx, ri, "kb/late.md", "Late", 0.90, nil)
	writeExplainFact(t, ctx, ri, "kb/r.md", "Root v2", 0.75, []string{"kb/early.md", "kb/late.md"})

	// Anchor at root v1: only early is referenced; late must not appear.
	facts := explainAll(t, ctx, "kb/r.md", cRoot1)

	root := findExpFact(facts, "kb/r.md")
	require.NotNil(t, root)
	require.Equal(t, "Root v1", root.Title, "root read as of the anchor commit")
	require.NotNil(t, findExpFact(facts, "kb/early.md"), "early ref present at the anchor")
	require.Nil(t, findExpFact(facts, "kb/late.md"), "a ref added after the anchor must not appear")

	// History bounded to the anchor: only the v1 creation, no v2 edit.
	require.NotNil(t, root.History)
	require.Len(t, root.History.Revisions, 1, "only the creation is in the anchor's ancestry")
	require.False(t, root.History.MoreAvailable)
}

// retractExplainFact deletes a fact at path on the test branch.
func retractExplainFact(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path string) {
	t.Helper()
	ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().DeleteFact(ctx, explainTestBranch, path, "retract "+path)
		require.NoError(t, err)
	})
}

// TestExplain_RetractedReferenceSurfacesDeleted pins that a reference whose
// target is retracted at HEAD still resolves at its pinned (pre-retraction)
// version, and is flagged deleted:true.
func TestExplain_RetractedReferenceSurfacesDeleted(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/gone.md", "Gone", 0.90, nil)
	writeExplainFact(t, ctx, ri, "kb/r.md", "R", 0.80, []string{"kb/gone.md"})
	retractExplainFact(t, ctx, ri, "kb/gone.md") // retract AFTER the reference

	facts := explainAll(t, ctx, "kb/r.md", "")
	gone := findExpFact(facts, "kb/gone.md")
	require.NotNil(t, gone, "retracted reference still resolves at its pinned version")
	require.Equal(t, "Gone", gone.Title, "reads the pre-retraction version M pointed to")
	require.True(t, gone.Deleted, "currently retracted at HEAD → deleted:true")
}

// TestExplain_SharedPathTwoVersionsBothSurface pins that the composite (path,
// commit) seen-key keeps two distinct versions of the same fact reached via
// different parents — they are different nodes in a versioned walk.
func TestExplain_SharedPathTwoVersionsBothSurface(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/l.md", "L-v1", 0.50, nil)
	writeExplainFact(t, ctx, ri, "kb/a.md", "A", 0.60, []string{"kb/l.md"}) // A → L@v1
	writeExplainFact(t, ctx, ri, "kb/l.md", "L-v2", 0.70, nil)
	writeExplainFact(t, ctx, ri, "kb/b.md", "B", 0.60, []string{"kb/l.md"}) // B → L@v2
	writeExplainFact(t, ctx, ri, "kb/root.md", "Root", 0.90, []string{"kb/a.md", "kb/b.md"})

	facts := explainAll(t, ctx, "kb/root.md", "")
	var titles []string
	for _, f := range facts {
		if f.Path == "kb/l.md" {
			titles = append(titles, f.Title)
		}
	}
	require.ElementsMatch(t, []string{"L-v1", "L-v2"}, titles,
		"both pinned versions of the shared leaf surface as distinct nodes")
}
