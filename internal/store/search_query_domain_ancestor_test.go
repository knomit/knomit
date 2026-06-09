package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedFactsWithDomains opens a fresh service and writes one pragmatic policy
// fact per (path, domain) pair. Returns the service for the caller's branch.
// Used by DomainAncestor filter tests where each test wants a specific set of
// domain values at known paths.
func seedFactsWithDomains(t *testing.T, branch string, factsByPath map[string]string) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	for path, domain := range factsByPath {
		_, err := svc.Facts().WriteFact(ctx, branch, path,
			pragmaticFactBody("t-"+path, "body", []string{domain}, nil),
			"add "+path, "")
		require.NoError(t, err)
	}
	return svc
}

// TestSearch_DomainAncestor_ExactAndParent verifies the new DomainAncestor
// filter matches the queried domain itself and any ancestor path, but never
// descendant or sibling paths.
//
// Fixture: facts with domains [store], [store/resolver], [store/cache], [ui].
// Query: DomainAncestor=[store/resolver].
// Expect: [kb/p/a.md (store), kb/p/b.md (store/resolver)] only.
func TestSearch_DomainAncestor_ExactAndParent(t *testing.T) {
	const branch = "main"
	svc := seedFactsWithDomains(t, branch, map[string]string{
		"kb/p/a.md": "store",
		"kb/p/b.md": "store/resolver",
		"kb/p/c.md": "store/cache",
		"kb/p/d.md": "ui",
	})

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{
		DomainAncestor: []string{"store/resolver"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/p/a.md", "kb/p/b.md"}, pathsOfResults(results))
}

// TestSearch_DomainAncestor_NoMatch verifies that querying for a path with no
// ancestor or exact match in the index returns an empty result.
func TestSearch_DomainAncestor_NoMatch(t *testing.T) {
	const branch = "main"
	svc := seedFactsWithDomains(t, branch, map[string]string{
		"kb/p/a.md": "ui",
		"kb/p/b.md": "bridge",
	})

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{
		DomainAncestor: []string{"store/resolver"},
	})
	require.NoError(t, err)
	require.Empty(t, pathsOfResults(results))
}

// TestSearch_DomainAncestor_DeepDescendant verifies the ancestor match works
// for queries arbitrarily deeper than the fact's domain — a fact with domain
// [store] should surface for any query path beginning "store/...".
func TestSearch_DomainAncestor_DeepDescendant(t *testing.T) {
	const branch = "main"
	svc := seedFactsWithDomains(t, branch, map[string]string{
		"kb/p/a.md": "store",
	})

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{
		DomainAncestor: []string{"store/resolver/cache/inner"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/p/a.md"}, pathsOfResults(results))
}

// TestSearch_DomainAncestor_GlobalDoesNotLeak guards against the SQL predicate
// accidentally matching unrelated short domains. `global` is not a path
// ancestor of `store/resolver`, so it must not appear in the result.
func TestSearch_DomainAncestor_GlobalDoesNotLeak(t *testing.T) {
	const branch = "main"
	svc := seedFactsWithDomains(t, branch, map[string]string{
		"kb/p/a.md": "global",
	})

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{
		DomainAncestor: []string{"store/resolver"},
	})
	require.NoError(t, err)
	require.Empty(t, pathsOfResults(results))
}
