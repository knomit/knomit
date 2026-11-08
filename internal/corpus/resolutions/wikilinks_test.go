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

// A {body} resolution is a fact write, so it gets knomit_update's wiki-link
// rule: the resolved body may hold no [[...]] outside code, whatever the
// merged versions (fork, parent tip, experiment tip) held.

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

func TestNormalize_WikiLinkRule_ResultingBodyOnly(t *testing.T) {
	// Every merged version holds [[a]]; the resolution may still not keep it.
	ri, exp := wikiRepo(t, "fork [[a]]", "parent [[a]] and [[b]]", "exp [[a]]")

	err := resolveWiki(t, ri, exp, wikiPath, "merged [[a]]")
	require.ErrorIs(t, err, knomitfact.ErrWikiLinkInBody, "keeping what the versions held is refused")
	require.Contains(t, err.Error(), `resolution body for "`+wikiPath+`"`)
	require.Contains(t, err.Error(), "including ones the fact already had")

	require.ErrorIs(t, resolveWiki(t, ri, exp, wikiPath, "merged [[c]]"), knomitfact.ErrWikiLinkInBody,
		"replacing them with another is refused")
	require.NoError(t, resolveWiki(t, ri, exp, wikiPath, "merged, plain"), "removing them is accepted")
}

// A path on no version is a fresh body: none allowed. Code and artifacts are
// exempt.
func TestNormalize_WikiLinkRule_FreshPathCodeAndArtifacts(t *testing.T) {
	ri, exp := wikiRepo(t, "x", "x", "x")

	err := resolveWiki(t, ri, exp, "kb/architecture/res/fresh.md", "fresh [[a]]")
	require.ErrorIs(t, err, knomitfact.ErrWikiLinkInBody)

	require.NoError(t, resolveWiki(t, ri, exp, "kb/architecture/res/fresh.md", "fresh `[[a]]`"))
	require.NoError(t, resolveWiki(t, ri, exp, "artifacts/runs/state.md", "saw [[link]]"))
}
