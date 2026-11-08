package resolutions

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/merge"
)

// A {body} resolution is a fact write, so it gets the wiki-link edit rule:
// it may hold as many [[...]] outside code as the merged version that held
// the most (fork, parent tip, experiment tip), never more.

const wikiPath = "kb/architecture/res/wiki.md"

func wikiFactContent(t *testing.T, path, body string) string {
	t.Helper()
	f := knomitfact.NewFact(path)
	f.Title = "Wiki"
	f.Body = body
	f.Type = knomitfact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Entities = []string{}
	content, err := knomitfact.SerializeFact(f)
	require.NoError(t, err)
	return content
}

// wikiRepo writes wikiPath on the parent with fork, opens an experiment from
// there, then rewrites it to parentTip on the parent and expTip on the
// experiment. It returns the repo and the experiment branch.
func wikiRepo(t *testing.T, fork, parentTip, expTip string) (*repos.RepoInstance, string) {
	t.Helper()
	ctx := context.Background()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, resParent))
	write := func(branch, body string) {
		_, err := svc.Facts().WriteFact(ctx, branch, wikiPath, wikiFactContent(t, wikiPath, body), "write "+branch, "")
		require.NoError(t, err)
	}
	write(resParent, fork)
	exp, err := svc.Merge().OpenExperiment(ctx, "wiki", "", resParent)
	require.NoError(t, err)
	write(resParent, parentTip)
	write(exp.Branch(), expTip)
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "res", UID: "res-wiki-repo", AgentBranch: resParent, Svc: svc,
		Ontology: knomitfact.CodeOntology(), OntologyRoot: "kb",
	}), exp.Branch()
}

func resolveWiki(t *testing.T, ri *repos.RepoInstance, exp, path, body string) error {
	t.Helper()
	_, err := Normalize(context.Background(), ri, resParent, exp,
		map[string]merge.Resolution{path: {Body: []byte(wikiFactContent(t, path, body))}})
	return err
}

func TestNormalize_WikiLinkEditRule_MaxOfTheMergedVersions(t *testing.T) {
	// The parent tip holds the most (two); the fork and the experiment one.
	ri, exp := wikiRepo(t, "fork [[a]]", "parent [[a]] and [[b]]", "exp [[a]]")

	require.NoError(t, resolveWiki(t, ri, exp, wikiPath, "merged [[a]] and [[b]]"),
		"keeping as many as the richest version is merging")
	require.NoError(t, resolveWiki(t, ri, exp, wikiPath, "merged, plain"), "dropping them is allowed")

	err := resolveWiki(t, ri, exp, wikiPath, "merged [[a]] [[b]] [[c]]")
	require.ErrorIs(t, err, knomitfact.ErrWikiLinkInBody)
	require.Contains(t, err.Error(), `resolution body for "`+wikiPath+`"`)
	require.Contains(t, err.Error(), "the edit adds [[c]]")
}

// The FORK version counts too: a resolution may restore what the fork had.
func TestNormalize_WikiLinkEditRule_ForkCounts(t *testing.T) {
	ri, exp := wikiRepo(t, "fork [[a]] and [[b]]", "parent, plain", "exp, plain")

	require.NoError(t, resolveWiki(t, ri, exp, wikiPath, "restored [[a]] and [[b]]"),
		"restoring the fork's body is choosing the base side")
	require.Error(t, resolveWiki(t, ri, exp, wikiPath, "restored [[a]] [[b]] [[c]]"))
}

// A path on no version is a fresh body: none allowed. Code and artifacts are
// exempt.
func TestNormalize_WikiLinkEditRule_FreshPathCodeAndArtifacts(t *testing.T) {
	ri, exp := wikiRepo(t, "x", "x", "x")

	err := resolveWiki(t, ri, exp, "kb/architecture/res/fresh.md", "fresh [[a]]")
	require.ErrorIs(t, err, knomitfact.ErrWikiLinkInBody)

	require.NoError(t, resolveWiki(t, ri, exp, "kb/architecture/res/fresh.md", "fresh `[[a]]`"))
	require.NoError(t, resolveWiki(t, ri, exp, "artifacts/runs/state.md", "saw [[link]]"))
}
