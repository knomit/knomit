// Package synthtest is test support for the synthesize package tree: the store
// fixtures, embedder stand-ins and environment builders that more than one of
// synthesize's packages' tests share. A white-box test of one package cannot
// import another package's _test.go file, so what is shared lives here.
//
// It imports "testing" and is for _test.go files only. It sits under
// synthesize/internal/, so no package outside the synthesize tree can import it,
// and it is on the forbidden list of archtest's TestHarnessNotLinkedIntoShippedBinaries
// so no shipped binary can link it either. It imports step and the store, never
// a package that has tests built on it, so a child's white-box test can use it
// without an import cycle: a test in package step would form one (synthtest
// imports step), so step's tests that use it are in package step_test.
package synthtest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// BareRefFixture is the localRepoID the sources and evidence fixtures pass. It
// is deliberately empty: every fixture seeds facts with BARE refs
// (kb/topic/x.md), and repo identity only ever matters for distinguishing a
// kb://<own-id>/… ref from a foreign one — so with bare refs the id changes
// nothing.
//
// Production never passes empty: refs are stored canonical, and classifying a
// canonical ref without the id reads it as foreign, which silently empties the
// lineage walk. That path is covered by
// TestLineageWalk_FollowsCanonicalSelfQualifiedRefs.
const BareRefFixture = ""

// NewSourcesTestRepo opens a store with an initialised repo on agent/test and
// returns it with that branch.
func NewSourcesTestRepo(t *testing.T) (*store.Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	return svc, "agent/test"
}

// SeedFactWithSources writes an observation carrying an explicit sources
// count, so a test can assert on a pooled total rather than on a count that
// happens to equal the number of inputs.
func SeedFactWithSources(t *testing.T, svc *store.Service, branch, path string, sources int) {
	t.Helper()
	SeedTypedFactWithSources(t, svc, branch, path, sources, fact.Observation)
}

// SeedTypedFactWithSources is SeedFactWithSources with an explicit fact type.
func SeedTypedFactWithSources(t *testing.T, svc *store.Service, branch, path string, sources int, typ fact.Type) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = typ
	f.Domain = []string{"test"}
	f.Confidence = 0.8
	f.Sources = sources
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// ReadSources parses the fact at path and returns its sources count.
func ReadSources(t *testing.T, svc *store.Service, branch, path string) int {
	t.Helper()
	rf, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	parsed, err := fact.ParseFact(path, rf.Content)
	require.NoError(t, err)
	return parsed.Sources
}

// ReadFact parses the fact at path on branch.
func ReadFact(t *testing.T, svc *store.Service, branch, path string) fact.Fact {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), branch, path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, res.Content)
	require.NoError(t, err)
	return f
}

// CollectWarns returns an onProgress sink plus a pointer to the warn messages
// it accumulates.
func CollectWarns() (func(step.ProgressEvent), *[]string) {
	var warns []string
	return func(e step.ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}, &warns
}

// SeedOriginFact writes a fact with an explicit origin, type, confidence,
// sources and refs — everything the lineage walk dispatches on.
func SeedOriginFact(t *testing.T, svc *store.Service, branch, path string,
	typ fact.Type, origin fact.Origin, confidence float64, sources int, refs []string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = path
	f.Body = "body of " + path
	f.Type = typ
	f.Origin = origin
	f.Domain = []string{"test"}
	f.Confidence = confidence
	f.Sources = sources
	f.Refs = refs
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}
