package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
)

// A symlinked ontology fails the repo with an error that names the path and
// says symlinks are not followed, at every place the repo reads its ontology
// (user ruling 2026-10-08, "d"). Never "no ontology", never the link text
// parsed. Each fixture's link TEXT is itself a parseable ontology, so a reader
// that read it as content would succeed and the test would go red.

const craftedLinkOntology = "id: crafted\nname: Crafted\ntopics:\n  notes:\n    description: N\n"

func requireSymlinkNamed(t *testing.T, err error, path string) {
	t.Helper()
	require.ErrorIs(t, err, fact.ErrSymlinkNotFollowed)
	require.Contains(t, err.Error(), path+" is a symlink")
	require.Contains(t, err.Error(), "does not follow symlinks")
}

// The OPEN path (identify stage, loadOntology): the repo opens readable but
// unwritable, and OntologyError names the symlink. The create-time check
// (branchHasOntology) gives the same error rather than "has one".
// Sabotage: loadOntology back on ReadFact, or treeOntologyFile back on
// IsFile → the crafted link text loads as the ontology → red.
func TestLoadOntology_SymlinkedOntologyFailsTheOpenNamingThePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	agentBranch := "agent/test-link"
	m := New(ctx, Deps{Cfg: config.Config{Home: dir}, AgentBranch: agentBranch})
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	_, err := svc.RawSymlinkForTest(ctx, agentBranch, OntologyPath, craftedLinkOntology, "symlink the ontology")
	require.NoError(t, err)

	has, err := branchHasOntology(ctx, svc, agentBranch)
	requireSymlinkNamed(t, err, OntologyPath)
	require.False(t, has)
	require.NoError(t, m.Close())

	m2 := New(ctx, Deps{Cfg: config.Config{Home: dir}, AgentBranch: agentBranch})
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })
	ri = m2.Get(testRepoName)
	require.NotNil(t, ri, "the repo still opens: its data stays reachable")
	requireSymlinkNamed(t, ri.OntologyError(), OntologyPath)
	require.Nil(t, ri.Ontology())
	require.False(t, ri.WritableBranch(agentBranch))
}

// An EMPTY .knomit/ontology.yaml is the ontology (the first rung present):
// a regular legacy rung does not stand in for it, and the error names the
// empty file rather than listing every rung. Sabotage: drop the empty-file
// branch → "no ontology at <all rungs>" → red; fall through to the legacy
// rung (the pre-#439 ReadFact walk) → the repo opens writable → red.
func TestLoadOntology_EmptyCanonicalDoesNotFallBackToLegacy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	agentBranch := "agent/test-empty"
	m := New(ctx, Deps{Cfg: config.Config{Home: dir}, AgentBranch: agentBranch})
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	legacy, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, agentBranch, LegacyOntologyPath, string(legacy), "seed legacy", "updated")
	require.NoError(t, err)
	_, err = svc.RawWriteForTest(ctx, agentBranch, OntologyPath, "", "empty the canonical ontology")
	require.NoError(t, err)
	p, data, err := svc.OntologyFileAt(ctx, agentBranch)
	require.NoError(t, err)
	require.Equal(t, OntologyPath, p, "the fixture holds an empty canonical file")
	require.Empty(t, data)
	require.NoError(t, m.Close())

	m2 := New(ctx, Deps{Cfg: config.Config{Home: dir}, AgentBranch: agentBranch})
	require.NoError(t, m2.Start())
	t.Cleanup(func() { _ = m2.Close() })
	ri = m2.Get(testRepoName)
	require.NotNil(t, ri)
	require.Error(t, ri.OntologyError())
	require.Contains(t, ri.OntologyError().Error(), OntologyPath+" on "+agentBranch+" is empty")
	require.Nil(t, ri.Ontology(), "the legacy rung does not stand in for the empty canonical file")
	require.False(t, ri.WritableBranch(agentBranch))
}

// The remote probe (the create wizard's "is this a knowledge base?") answers
// UNKNOWN with the named error as its detail, and a clone of that remote is
// refused with the same error. Sabotage: drop the probe's symlink clause →
// "yes" → red.
func TestProbeInitialized_SymlinkedOntologyIsUnknownNamingThePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := sourceRepo(t, dir, "remote", sourceSpec{
		noOntol: true,
		files:   map[string]string{"real.yaml": craftedLinkOntology},
		links:   map[string]string{OntologyPath: "../real.yaml"},
	})
	m := newRemoteModeManager(t, dir)

	got, err := m.ProbeInitialized(ctx, OriginSpec{URL: url, Branch: "main"})
	require.NoError(t, err)
	require.Equal(t, InitializedUnknown, got.Initialized)
	require.Contains(t, got.Detail, OntologyPath+" is a symlink")
	require.Contains(t, got.Detail, "does not follow symlinks")

	_, err = m.Create(ctx, CreateSpec{Name: "linked", Mode: "clone", Origin: &OriginSpec{URL: url}}, nil)
	requireSymlinkNamed(t, err, OntologyPath)
	require.Nil(t, m.Get("linked"))
}

// A template whose .knomit/ontology.yaml is a symlink is refused with
// ErrTemplateNotRegular AND the same named symlink error.
func TestCreateFromTemplate_SymlinkedOntologyNamesThePath(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	url := sourceRepo(t, root, "src", sourceSpec{
		files: map[string]string{".knomit/templates/linked/real.yaml": craftedLinkOntology},
		links: map[string]string{".knomit/templates/linked/.knomit/ontology.yaml": "../real.yaml"},
	})
	mount(t, m, "src", "clone", url)
	spec := CreateSpec{Name: "new-repo", Mode: "template", Template: &TemplateRef{Repo: "src", Name: "linked"}}
	err := m.CreatePreflight(context.Background(), spec)
	require.ErrorIs(t, err, ErrTemplateNotRegular)
	requireSymlinkNamed(t, err, ".knomit/ontology.yaml")
}

// The guidance reader: a symlinked ontology at the consensus tip is warned
// ONCE per commit with the named error and gives no guidance; a symlinked
// guidance file is "does not exist there", like an absent one.
func TestConsensusGuidance_Symlinks(t *testing.T) {
	ctx := context.Background()
	t.Run("symlinked ontology", func(t *testing.T) {
		g := newGuidanceRepo(t)
		w := captureGuidanceWarnings(t)
		_, err := g.svc.RawSymlinkForTest(ctx, "main", OntologyPath, guidanceOntologyMain, "symlink")
		require.NoError(t, err)
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Nil(t, g.ri.ConsensusGuidance(ctx))
		require.Len(t, w.all(), 1, "once per commit: %v", w.all())
		require.Contains(t, w.all()[0], OntologyPath+" is a symlink")
	})
	t.Run("symlinked guidance file", func(t *testing.T) {
		g := newGuidanceRepo(t)
		w := captureGuidanceWarnings(t)
		g.put(t, "main", ".knomit/ontology.yaml", guidanceOntologyMain)
		g.put(t, "main", ".knomit/guidance/forecast.md", "MAIN FORECAST\n")
		_, err := g.svc.RawSymlinkForTest(ctx, "main", ".knomit/guidance/all.md", "MAIN ALL\n", "symlink")
		require.NoError(t, err)
		cg := g.ri.ConsensusGuidance(ctx)
		require.NotNil(t, cg)
		_, ok := cg.Text("guidance/all.md")
		require.False(t, ok, "the symlink's text is not guidance")
		text, ok := cg.Text("guidance/forecast.md")
		require.True(t, ok)
		require.Equal(t, "MAIN FORECAST\n", text)
		found := false
		for _, m := range w.all() {
			if strings.Contains(m, ".knomit/guidance/all.md") && strings.Contains(m, "the file does not exist there") {
				found = true
			}
		}
		require.True(t, found, "a symlinked guidance file reads as absent: %v", w.all())
	})
}
