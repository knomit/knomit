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
	Path       string  `json:"path"`
	Commit     string  `json:"commit"`
	Depth      int     `json:"depth"`
	Title      string  `json:"title"`
	Type       string  `json:"type"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
	// Sources is a pointer so a test can tell "key absent" from "present and
	// zero" — the exact distinction the omitempty on explainFactEntry erased.
	Sources    *int        `json:"sources"`
	Body       string      `json:"body"`
	Summary    bool        `json:"summary"`
	Deleted    bool        `json:"deleted"`
	Superseded bool        `json:"superseded"`
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
	return writeExplainFactSources(t, ctx, ri, path, title, conf, 1, refs)
}

// writeExplainFactSources is writeExplainFact with an explicit sources count.
// The default of 1 hides the sources: 0 case, which is the common one in real
// corpora and the one the omitempty defect made invisible.
func writeExplainFactSources(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path, title string, conf float64, sources int, refs []string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = title + " body text."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = conf
	f.Sources = sources
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

// TestExplain_SummaryNodesCarrySources pins that sources reaches summary nodes,
// not just the root. instructions.go requires a hypothesis evidence chain to cite
// "confidence/sources for each cited fact", and those cited facts arrive from the
// provenance walk as summaries — so a summary carrying confidence but no sources
// asks the agent for a number the tool never returned.
func TestExplain_SummaryNodesCarrySources(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFactSources(t, ctx, ri, "kb/child.md", "Child", 0.88, 3, nil)
	writeExplainFactSources(t, ctx, ri, "kb/root.md", "Root", 0.90, 2, []string{"kb/child.md"})

	facts := explainAll(t, ctx, "kb/root.md", "")

	child := findExpFact(facts, "kb/child.md")
	require.NotNil(t, child)
	require.True(t, child.Summary, "kb/child.md is a summary node")
	require.NotNil(t, child.Sources, "summary nodes must carry sources alongside confidence")
	require.Equal(t, 3, *child.Sources, "summary reports the fact's own sources count")
}

// TestExplain_ZeroSourcesIsSerialized pins that sources: 0 is emitted rather than
// dropped. Zero is a claim — "no independent sources recorded" — not an absence.
// fact.Fact serializes sources without omitempty (internal/fact/format.go), so
// knomit_query already reports "sources":0 for these facts; omitting it here made
// the two tools disagree about the same field on the same corpus.
func TestExplain_ZeroSourcesIsSerialized(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFactSources(t, ctx, ri, "kb/child.md", "Child", 0.88, 0, nil)
	writeExplainFactSources(t, ctx, ri, "kb/root.md", "Root", 0.90, 0, []string{"kb/child.md"})

	facts := explainAll(t, ctx, "kb/root.md", "")

	root := findExpFact(facts, "kb/root.md")
	require.NotNil(t, root)
	require.NotNil(t, root.Sources, "root must emit sources: 0, not drop the key")
	require.Equal(t, 0, *root.Sources)

	child := findExpFact(facts, "kb/child.md")
	require.NotNil(t, child)
	require.NotNil(t, child.Sources, "summary must emit sources: 0, not drop the key")
	require.Equal(t, 0, *child.Sources)
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
func retractExplainFact(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path string) string {
	t.Helper()
	var commit string
	ri.WithRead(func(svc *store.Service) {
		c, err := svc.Facts().DeleteFact(ctx, explainTestBranch, path, "retract "+path)
		require.NoError(t, err)
		commit = c
	})
	return commit
}

// TestExplain_AnchoredAtRetractionGapFallsBack regresses the case where readNode
// dropped a node whose pinned version is unreadable but the fact is LIVE at HEAD.
// A fact created → retracted → re-added, then explained AS OF the retraction-gap
// commit: ReadFact{AtCommit: gap} fails (the file is absent in that tree), so the
// walk must fall back to the most recent version before the gap rather than
// erroring. Previously the live-at-HEAD branch returned ok=false and the root
// surfaced "could not read".
func TestExplain_AnchoredAtRetractionGapFallsBack(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/res.md", "Res-v1", 0.50, nil)
	gap := retractExplainFact(t, ctx, ri, "kb/res.md")             // file absent as of `gap`
	writeExplainFact(t, ctx, ri, "kb/res.md", "Res-v3", 0.70, nil) // re-added → live at HEAD

	facts := explainAll(t, ctx, "kb/res.md", gap)
	root := findExpFact(facts, "kb/res.md")
	require.NotNil(t, root, "unreadable pin falls back to the pre-gap version instead of dropping")
	require.Equal(t, "Res-v1", root.Title, "falls back to the most recent version before the gap")
	require.False(t, root.Deleted, "re-added fact is live at HEAD → deleted:false")
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

// TestExplain_SupersededReferenceFlagged pins that a child whose source is
// still live at HEAD but has a NEWER revision than the version the referrer
// pinned is flagged superseded:true (and not deleted). A sibling whose source
// never changed after the edge formed is neither superseded nor deleted.
func TestExplain_SupersededReferenceFlagged(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFact(t, ctx, ri, "kb/moved.md", "Moved-v1", 0.60, nil)
	writeExplainFact(t, ctx, ri, "kb/stable.md", "Stable", 0.70, nil)
	// Root pins moved@v1 and stable@v1 as of this write.
	writeExplainFact(t, ctx, ri, "kb/root.md", "Root", 0.90, []string{"kb/moved.md", "kb/stable.md"})
	// moved evolves AFTER the edge formed; stable does not.
	writeExplainFact(t, ctx, ri, "kb/moved.md", "Moved-v2", 0.80, nil)

	facts := explainAll(t, ctx, "kb/root.md", "")

	moved := findExpFact(facts, "kb/moved.md")
	require.NotNil(t, moved)
	require.Equal(t, "Moved-v1", moved.Title, "child reads the pinned version the referrer saw")
	require.True(t, moved.Superseded, "source is live but its HEAD revision is newer → superseded:true")
	require.False(t, moved.Deleted, "live source is not deleted")

	stable := findExpFact(facts, "kb/stable.md")
	require.NotNil(t, stable)
	require.False(t, stable.Superseded, "unchanged source is not superseded")
	require.False(t, stable.Deleted)
}

// TestExplainResume_RejectsForeignBinding pins the binding-pinned cursor rule
// (lenses RFC §7.3): an explain cursor minted under one binding must not resume
// under a different binding, even one backed by the same store and branch. The
// rejection is deliberately indistinguishable from real expiry.
func TestExplainResume_RejectsForeignBinding(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	// A root with a child so the first explain leaves the queue non-empty and
	// returns a cursor.
	writeExplainFact(t, ctx, ri, "kb/child.md", "Child", 0.88, nil)
	writeExplainFact(t, ctx, ri, "kb/root.md", "Root", 0.90, []string{"kb/child.md"})

	// Mint the cursor through the normal handler path (binding name = "test").
	var first mcpgo.CallToolRequest
	first.Params.Arguments = map[string]any{"file": "kb/root.md"}
	firstResult, err := ExplainHandler()(ctx, first)
	require.NoError(t, err)
	require.False(t, firstResult.IsError, "first explain call failed: %s", resultText(t, firstResult))
	var firstResp expResp
	require.NoError(t, json.Unmarshal([]byte(resultText(t, firstResult)), &firstResp))
	require.NotNil(t, firstResp.Cursor, "explain must return a cursor while the walk has more")

	// Capture the shared store so the foreign binding hits the same session DB.
	var svc *store.Service
	ri.WithRead(func(s *store.Service) { svc = s })

	// Resume under a DIFFERENT binding backed by the same store and branch.
	foreignRI := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "other-lens",
		UID:          nextTestRepoUID(),
		AgentBranch:  explainTestBranch,
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
	})
	foreignCtx := repos.WithRepoInstance(context.Background(), foreignRI)

	var resume mcpgo.CallToolRequest
	resume.Params.Arguments = map[string]any{"file": "kb/root.md", "cursor": *firstResp.Cursor}
	result, err := ExplainHandler()(foreignCtx, resume)
	require.NoError(t, err)
	require.True(t, result.IsError, "foreign-binding resume must be rejected")
	require.Contains(t, resultText(t, result), "session expired or not found",
		"foreign-binding rejection must be indistinguishable from expiry")
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

// TestExplain_DiamondGraphNoDuplicateAcrossPages regresses B.7: a diamond
// (root → A, root → B, A → B, with B pinned at the SAME commit via both edges)
// must emit each node exactly once across all cursor pages. Pre-fix, mint
// enqueued A and B but seeded the seen-set with the root alone, so resume
// re-enqueued B when it walked A→B and B surfaced twice across pages.
func TestExplain_DiamondGraphNoDuplicateAcrossPages(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	// Write order makes root→B and A→B both pin B at the same commit (cB).
	writeExplainFact(t, ctx, ri, "kb/b.md", "B", 0.60, nil)
	writeExplainFact(t, ctx, ri, "kb/a.md", "A", 0.70, []string{"kb/b.md"})
	writeExplainFact(t, ctx, ri, "kb/root.md", "Root", 0.90, []string{"kb/a.md", "kb/b.md"})

	facts := explainAll(t, ctx, "kb/root.md", "")

	// Count every returned (path, commit) node across ALL pages.
	counts := map[string]int{}
	for _, f := range facts {
		counts[f.Path+"@"+f.Commit]++
	}
	for key, n := range counts {
		require.Equalf(t, 1, n,
			"node %s emitted %d times across cursor pages; each node must appear exactly once", key, n)
	}
	require.NotNil(t, findExpFact(facts, "kb/b.md"), "the shared leaf must surface in the walk")
}

// TestClassifyRefs_ForeignKbSchemeIsExternal pins the bucketing contract: a
// bare "*.md" ref is a local fact edge, and a kb:// ref naming ANOTHER repo is
// a cross-repo pointer and therefore External. What changed is that "another
// repo" is now decided by comparing ids, not by the presence of the scheme —
// see TestClassifyRefs_SelfQualifiedIsLocal.
func TestClassifyRefs_ForeignKbSchemeIsExternal(t *testing.T) {
	const localID = "3ec012f5b4d2"
	cr := classifyRefs([]string{
		"kb://3f9a2c1e8b7d/kb/a/b.md", // FOREIGN id → cross-repo pointer
		"kb/a/b.md",                   // bare local fact edge
		"https://x",                   // external
		"src://s/p@c",                 // source citation, never a fact edge
	}, localID)
	require.Equal(t, []string{"kb/a/b.md"}, cr.Local,
		"only the bare .md ref is a local fact edge here")
	require.Equal(t, []string{"kb://3f9a2c1e8b7d/kb/a/b.md", "https://x", "src://s/p@c"}, cr.External,
		"a foreign kb://, an external URL, and a source ref are all External")
}

// A kb://<own-id>/… ref is the documented canonical form for a fact in THIS
// repo, so it is a local edge — not a cross-repo pointer. The old rule sent
// every kb:// ref to External, so writing a ref in canonical form produced no
// edge and explain reported it as external.
func TestClassifyRefs_SelfQualifiedIsLocal(t *testing.T) {
	const localID = "3ec012f5b4d2"
	cr := classifyRefs([]string{"kb://3ec012f5b4d2/kb/a/b.md"}, localID)
	require.Equal(t, []string{"kb/a/b.md"}, cr.Local,
		"a self-qualified ref is local, and is reported by its repo-relative path")
	require.Empty(t, cr.External)
}

// Both buckets must serialize as [] rather than null.
func TestClassifyRefs_EmptySlicesAreNonNil(t *testing.T) {
	cr := classifyRefs(nil, "3ec012f5b4d2")
	require.NotNil(t, cr.Local)
	require.NotNil(t, cr.External)
	require.Empty(t, cr.Local)
	require.Empty(t, cr.External)
}

// expMotif mirrors the explainMotif wire shape. cluster_key and member_count
// are what let an agent go fact → cluster → REST detail / precise pivot; the
// pointer on MemberCount distinguishes "key absent" from "present and zero".
type expMotif struct {
	Motif       string   `json:"motif"`
	Canonical   string   `json:"canonical"`
	Definition  string   `json:"definition"`
	DF          int      `json:"df"`
	ClusterKey  string   `json:"cluster_key"`
	MemberCount *int     `json:"member_count"`
	Siblings    []string `json:"siblings"`
}

// writeExplainFactMotifs is writeExplainFact with motifs attached.
func writeExplainFactMotifs(t *testing.T, ctx context.Context, ri *repos.RepoInstance, path, title string, motifs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = title + " body text."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.9
	f.Sources = 1
	f.Entities = []string{}
	f.Motifs = motifs
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	ri.WithRead(func(svc *store.Service) {
		_, werr := svc.Facts().WriteFact(ctx, explainTestBranch, path, content, "write "+path, "")
		require.NoError(t, werr)
	})
}

// explainMotifsOf re-reads the raw explain response for the motif block, which
// the expFact mirror deliberately does not carry.
func explainMotifsOf(t *testing.T, ctx context.Context, file string) []expMotif {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": file}
	result, err := ExplainHandler()(ctx, req)
	require.NoError(t, err)
	text := resultText(t, result)
	require.False(t, result.IsError, "explain error: %s", text)

	var resp struct {
		Facts []struct {
			Path   string     `json:"path"`
			Motifs []expMotif `json:"motifs"`
		} `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp), "bad JSON: %s", text)
	for _, f := range resp.Facts {
		if f.Path == file {
			return f.Motifs
		}
	}
	t.Fatalf("explain did not return %s: %s", file, text)
	return nil
}

// The explain motif block carries the cluster's STABLE key and how many
// spellings resolve to it — an agent can then pivot precisely (GET
// /motifs/{cluster_key}) instead of guessing from the canonical, which flips.
func TestExplainMotifs_CarryClusterKeyAndMemberCount(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	ctx := repos.WithRepoInstance(context.Background(), ri)

	writeExplainFactMotifs(t, ctx, ri, "kb/one.md", "Drift one", []string{"config-drift"})
	writeExplainFactMotifs(t, ctx, ri, "kb/two.md", "Drift two", []string{"config-drifts"})
	writeExplainFactMotifs(t, ctx, ri, "kb/three.md", "Lonely", []string{"silent-fallback"})

	var wantKey string
	ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Motifs().RebuildAliases(ctx, explainTestBranch))
		k, err := svc.Motifs().ClusterKey(ctx, explainTestBranch, "config-drift")
		require.NoError(t, err)
		wantKey = k
	})
	require.NotEmpty(t, wantKey)

	motifs := explainMotifsOf(t, ctx, "kb/one.md")
	require.Len(t, motifs, 1)
	require.Equal(t, "config-drift", motifs[0].Motif)
	require.Equal(t, wantKey, motifs[0].ClusterKey,
		"cluster_key must be the store's stable key, not the spelling or canonical")
	require.NotNil(t, motifs[0].MemberCount)
	require.Equal(t, 2, *motifs[0].MemberCount,
		"both spellings resolve to this cluster")

	// A cluster nothing was aliased into is the singleton it degrades to
	// everywhere else — 1, never 0 or absent.
	single := explainMotifsOf(t, ctx, "kb/three.md")
	require.Len(t, single, 1)
	require.NotNil(t, single[0].MemberCount)
	require.Equal(t, 1, *single[0].MemberCount)
}
