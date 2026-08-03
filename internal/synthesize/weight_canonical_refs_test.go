package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// THE BUG THIS PINS. Refs are STORED canonical — knomit_learn qualifies every
// local ref to kb://<own-id>/<path> on write. The lineage walk reads facts back
// from the store, so by the time it sees a ref it is in that form. Classifying
// it with an empty localRepoID reads it as FOREIGN, so the walk finds no
// ancestors at all: every derived fact silently falls through to counting its
// own mass instead of composing through the evidence beneath it.
//
// Nothing errors and no test fails on the arithmetic — the weight is simply
// computed from the wrong set. Hence a test that asserts the two ref FORMS are
// interchangeable, rather than one that asserts a magic number.
func TestLocalFactRefPaths_CanonicalSelfQualifiedIsLocal(t *testing.T) {
	const localID = "3ec012f5b4d2"
	in := []string{
		"kb/technology/bare.md",
		fact.QualifyKBPath(localID, "kb/technology/canonical.md"),
		fact.QualifyKBPath("7b4887ce51d9", "kb/technology/foreign.md"),
		"src://knomit/internal/x.go@ca1c272",
		"https://example.com/x",
	}
	require.Equal(t,
		[]string{"kb/technology/bare.md", "kb/technology/canonical.md"},
		localFactRefPaths(in, localID),
		"a self-qualified ref is a local edge and must be returned by repo-relative path")

	require.Equal(t, []string{"kb/technology/bare.md"}, localFactRefPaths(in, ""),
		"with no identity a kb:// ref reads as foreign — which is why the id must be threaded")
}

// The end-to-end consequence: a two-level lineage whose refs are stored in
// canonical form must weigh the same as one stored bare. If the walk stops
// following canonical refs, the derived fact's weight collapses to its own mass
// and the two stop matching.
func TestLineageWalk_FollowsCanonicalSelfQualifiedRefs(t *testing.T) {
	ctx := context.Background()
	svc, branch := newSourcesTestRepo(t)

	root, err := svc.RootCommit(ctx, branch)
	require.NoError(t, err)
	localRepoID := fact.ID12(root)

	// Ground truth: one observation carrying the actual corroborations.
	seedFactWithSources(t, svc, branch, "kb/technology/ground.md", 4)

	// Two derived facts over the SAME ground truth, differing only in how their
	// ref is spelled.
	seedDerivedCiting(t, svc, branch, "kb/technology/derived-bare.md",
		"kb/technology/ground.md")
	seedDerivedCiting(t, svc, branch, "kb/technology/derived-canon.md",
		fact.QualifyKBPath(localRepoID, "kb/technology/ground.md"))

	bare := computeWeight(ctx, svc.Facts(), branch, localRepoID,
		[]string{"kb/technology/derived-bare.md"})
	canon := computeWeight(ctx, svc.Facts(), branch, localRepoID,
		[]string{"kb/technology/derived-canon.md"})

	require.Greater(t, bare, 0.0, "the bare-ref lineage must resolve (control)")
	require.InDelta(t, bare, canon, 1e-9,
		"the canonical stored form must weigh identically to the bare form — "+
			"if it does not, the walk stopped following local refs the moment they were qualified")
}

// seedDerivedCiting writes a distilled fact whose single ref is `ref`. Distilled
// origin is what makes collectEvidence descend into the lineage rather than
// counting the fact itself.
func seedDerivedCiting(t *testing.T, svc *store.Service, branch, path, ref string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = fact.Synthesis
	f.Origin = fact.Distilled
	f.Domain = []string{"test"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Refs = []string{ref}
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, path, body, "seed "+path, "")
	require.NoError(t, err)
}
