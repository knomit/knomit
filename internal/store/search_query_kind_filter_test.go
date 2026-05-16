package store

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// heuristicFactBody builds a serialized pragmatic heuristic fact body. Used
// alongside pragmaticFactBody (policy) and testFactBody (epistemic
// observation) to construct mixed-kind fixtures for the IncludeKinds filter.
func heuristicFactBody(title string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = "h"
	f.Kind = fact.Pragmatic
	f.Type = fact.Heuristic
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"test"}
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// seedMixedKindFacts writes one epistemic observation, one pragmatic policy,
// and one pragmatic heuristic to the given branch. Returns the service.
// Designed for IncludeKinds/IncludeTypes filter tests where every test wants
// the same three-fact fixture.
func seedMixedKindFacts(t *testing.T, branch string) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/obs/a.md",
		testFactBody("epistemic obs", 0.9, nil), "add obs", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/policy/p.md",
		pragmaticFactBody("Use TLS", "All cross-host traffic must use TLS 1.3+.",
			[]string{"test"}, nil), "add policy", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/heuristic/h.md",
		heuristicFactBody("Prefer small PRs"), "add heuristic", "")
	require.NoError(t, err)
	return svc
}

func pathsOfResults(rs []SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	sort.Strings(out)
	return out
}

// TestSearchQuery_IncludeKinds_PragmaticOnly verifies that IncludeKinds
// filters to pragmatic facts (policy + heuristic) and excludes the epistemic
// observation.
func TestSearchQuery_IncludeKinds_PragmaticOnly(t *testing.T) {
	const branch = "main"
	svc := seedMixedKindFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchQuery{
		IncludeKinds: []string{"pragmatic"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/heuristic/h.md", "kb/policy/p.md"}, pathsOfResults(results))
	for _, r := range results {
		require.Equal(t, "pragmatic", r.Kind, "every result must be pragmatic")
	}
}

// TestSearchQuery_IncludeKinds_EpistemicOnly verifies that IncludeKinds
// filters to epistemic facts and excludes both pragmatic types.
func TestSearchQuery_IncludeKinds_EpistemicOnly(t *testing.T) {
	const branch = "main"
	svc := seedMixedKindFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchQuery{
		IncludeKinds: []string{"epistemic"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/obs/a.md"}, pathsOfResults(results))
	require.Equal(t, "epistemic", results[0].Kind)
}

// TestSearchQuery_IncludeKinds_Both verifies that listing both kinds returns
// every fact — equivalent to no filter, but exercises the multi-value
// placeholder expansion in newFactFilter.
func TestSearchQuery_IncludeKinds_Both(t *testing.T) {
	const branch = "main"
	svc := seedMixedKindFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchQuery{
		IncludeKinds: []string{"epistemic", "pragmatic"},
	})
	require.NoError(t, err)
	require.Equal(t,
		[]string{"kb/heuristic/h.md", "kb/obs/a.md", "kb/policy/p.md"},
		pathsOfResults(results))
}

// TestSearchQuery_IncludeKinds_Empty verifies that an absent IncludeKinds
// filter behaves as "all kinds" — the SQL clause is suppressed entirely.
func TestSearchQuery_IncludeKinds_Empty(t *testing.T) {
	const branch = "main"
	svc := seedMixedKindFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchQuery{})
	require.NoError(t, err)
	require.Equal(t,
		[]string{"kb/heuristic/h.md", "kb/obs/a.md", "kb/policy/p.md"},
		pathsOfResults(results))
}

// TestSearchQuery_IncludeKinds_AndIncludeTypes verifies that IncludeKinds and
// IncludeTypes AND-combine. With IncludeKinds=[pragmatic] and
// IncludeTypes=[policy], only the pragmatic policy fact survives; the
// pragmatic heuristic is filtered out by the type clause and the epistemic
// observation by the kind clause.
func TestSearchQuery_IncludeKinds_AndIncludeTypes(t *testing.T) {
	const branch = "main"
	svc := seedMixedKindFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchQuery{
		IncludeKinds: []string{"pragmatic"},
		IncludeTypes: []string{"policy"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/policy/p.md"}, pathsOfResults(results))
	require.Equal(t, "pragmatic", results[0].Kind)
	require.Equal(t, "policy", results[0].Type)
}
