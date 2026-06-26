package synthesize

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// seedFactInDomain writes one observation fact in the given domain on branch.
func seedFactInDomain(t *testing.T, svc *store.Service, branch, slug, domain string) {
	t.Helper()
	f := fact.NewFact("kb/" + domain + "/" + slug + ".md")
	f.Title = slug
	f.Body = "body of " + slug
	f.Type = fact.Observation
	f.Domain = []string{domain}
	f.Confidence = 0.5
	f.Sources = 1
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// TestScopeFilter_FirstRun_RestrictsToDomain confirms the first-run seed
// search (no watermark) applies the scope filter's domain — only facts in
// the requested domain become seeds.
func TestScopeFilter_FirstRun_RestrictsToDomain(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedFactInDomain(t, svc, branch, "a", "auth")
	seedFactInDomain(t, svc, branch, "b", "auth")
	seedFactInDomain(t, svc, branch, "c", "billing")
	seedFactInDomain(t, svc, branch, "d", "billing")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	r := NewReviewerWithOptions(ri, nil, EffortNormal, ScopeFilter{Domain: []string{"auth"}})

	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(context.Background(), branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	require.Len(t, seeds, 2, "scope=auth must restrict the seed pool")
	for _, s := range seeds {
		require.Contains(t, s.Domain, "auth", "seed %q must be in domain auth", s.File)
	}
}

// TestScopeFilter_Empty_WholeCorpus asserts the empty filter is whole-corpus —
// not a hidden "match nothing" footgun.
func TestScopeFilter_Empty_WholeCorpus(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	branch := "agent/test"

	seedFactInDomain(t, svc, branch, "a", "auth")
	seedFactInDomain(t, svc, branch, "b", "billing")
	seedFactInDomain(t, svc, branch, "c", "ops")

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  branch,
		Svc:          svc,
		OntologyRoot: "kb",
	})

	r := NewReviewerWithOptions(ri, nil, EffortNormal, ScopeFilter{}) // empty

	gs, idx, pipelineIdx, _ := r.storeIndices()
	seeds, err := r.dirtyFacts(context.Background(), branch, gs, idx, pipelineIdx)
	require.NoError(t, err)
	require.Len(t, seeds, 3, "empty scope = whole-corpus search")
}

// TestScopeFilter_IsEmpty asserts the predicate is the union zero.
func TestScopeFilter_IsEmpty(t *testing.T) {
	require.True(t, ScopeFilter{}.IsEmpty())
	require.False(t, ScopeFilter{Domain: []string{"x"}}.IsEmpty())
	require.False(t, ScopeFilter{Entities: []string{"y"}}.IsEmpty())
}
