package resolutions

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// #428 follow-up N1: a {body} resolution that ADDS a ref to a .knomit/ file
// is checked against the tree on EITHER side of the merge — the parent tip or
// the experiment tip — like a local fact ref. A file on neither side, or a
// symlink (never followed, N4), is refused.

const (
	resBoth       = ".knomit/skills/both.md"
	resParentOnly = ".knomit/skills/parent-only.md"
	resExpOnly    = ".knomit/skills/exp-only.md"
	resMissing    = ".knomit/skills/missing.md"
	resSymlink    = ".knomit/skills/link.md" // -> both.md, a regular file beside it
	resParent     = "agent/test"
	resExp        = "agent/side" // the experiment side; any branch name serves Normalize
	resFactPath   = "kb/architecture/res/a.md"
)

func resolutionRepo(t *testing.T) *repos.RepoInstance {
	t.Helper()
	ctx := context.Background()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(ctx, map[string]string{resBoth: "both\n"}, resParent))
	require.NoError(t, svc.Branches().CreateBranch(ctx, resExp, resParent))
	_, err = svc.RawWriteForTest(ctx, resParent, resParentOnly, "parent\n", "parent side only")
	require.NoError(t, err)
	_, err = svc.RawWriteForTest(ctx, resExp, resExpOnly, "experiment\n", "experiment side only")
	require.NoError(t, err)
	_, err = svc.RawSymlinkForTest(ctx, resParent, resSymlink, "both.md", "a symlink pushed through git")
	require.NoError(t, err)
	_, err = svc.RawSymlinkForTest(ctx, resExp, resSymlink, "both.md", "the same symlink on the experiment")
	require.NoError(t, err)
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "res", UID: "res-test-repo", AgentBranch: resParent, Svc: svc,
		Ontology: knomitfact.CodeOntology(), OntologyRoot: "kb",
	})
}

func resolutionBody(t *testing.T, ref string) []byte {
	t.Helper()
	f := knomitfact.NewFact(resFactPath)
	f.Title = "Resolution citing a system file"
	f.Body = "The merged version cites a file under .knomit/."
	f.Type = knomitfact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Entities = []string{}
	f.Refs = []string{ref}
	content, err := knomitfact.SerializeFact(f)
	require.NoError(t, err)
	return []byte(content)
}

// TestNormalize_SystemFileRefMustExistOnEitherSide: a ref to a file on both
// sides, on the parent only, and on the experiment only is accepted and
// canonicalised; a missing file and a symlink are refused with the gate's
// .knomit/ section.
// Sabotage: resolveGate's file resolver returns true always → missing and
// symlink accepted → red; false always → every accept case refused → red;
// the either-side loop checks only one branch → parent-only or exp-only
// refused → red.
func TestNormalize_SystemFileRefMustExistOnEitherSide(t *testing.T) {
	ri := resolutionRepo(t)
	ctx := context.Background()
	own := "kb://" + knomitfact.ID12(ri.ID()) + "/"

	for _, ref := range []string{resBoth, resParentOnly, resExpOnly} {
		out, err := Normalize(ctx, ri, resParent, resExp,
			map[string]store.Resolution{resFactPath: {Body: resolutionBody(t, ref)}})
		require.NoErrorf(t, err, "a resolution citing %s", ref)
		require.Containsf(t, string(out[resFactPath].Body), own+ref, "%s stored canonical", ref)
	}
	for _, ref := range []string{resMissing, resSymlink} {
		_, err := Normalize(ctx, ri, resParent, resExp,
			map[string]store.Resolution{resFactPath: {Body: resolutionBody(t, ref)}})
		require.Errorf(t, err, "a resolution citing %s", ref)
		require.Containsf(t, err.Error(), "does not exist under .knomit/ at the tip", "a resolution citing %s", ref)
		require.Containsf(t, err.Error(), ref, "the refusal names %s", ref)
	}
}
