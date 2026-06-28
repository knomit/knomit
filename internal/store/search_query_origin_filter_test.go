package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// discoveredFactBody builds a serialized synthesis fact body whose origin
// records "discovered" — the emergent-fact case the discovery engine will
// emit. Used alongside synthesisFactBody (distilled) and testFactBody
// (authored observation) for the IncludeOrigins filter fixture.
func discoveredFactBody(title string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = "discovered body"
	f.Type = fact.Synthesis
	f.Origin = fact.Discovered
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"test"}
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// seedMixedOriginFacts writes one authored observation, one distilled
// synthesis, and one discovered synthesis to branch, returning the service.
func seedMixedOriginFacts(t *testing.T, branch string) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/obs/a.md",
		testFactBody("authored obs", 0.9, nil), "add obs", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/synth/d.md",
		synthesisFactBody("distilled synth"), "add synth", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/synth/e.md",
		discoveredFactBody("discovered synth"), "add discovered", "")
	require.NoError(t, err)
	return svc
}

// TestSearchOptions_IncludeOrigins_DiscoveredOnly verifies that IncludeOrigins
// filters to discovered facts and excludes both authored and distilled rows.
func TestSearchOptions_IncludeOrigins_DiscoveredOnly(t *testing.T) {
	const branch = "main"
	svc := seedMixedOriginFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{
		IncludeOrigins: []string{"discovered"},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "kb/synth/e.md", results[0].Path)
}

// TestSearchOptions_IncludeOrigins_Multi verifies that a multi-value filter
// returns every matching origin (distilled + discovered) but still excludes
// the authored fact — the same multi-placeholder path used for kinds.
func TestSearchOptions_IncludeOrigins_Multi(t *testing.T) {
	const branch = "main"
	svc := seedMixedOriginFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{
		IncludeOrigins: []string{"distilled", "discovered"},
	})
	require.NoError(t, err)
	require.Equal(t,
		[]string{"kb/synth/d.md", "kb/synth/e.md"},
		pathsOfResults(results))
}

// TestSearchOptions_IncludeOrigins_Empty verifies that an absent IncludeOrigins
// filter behaves as "all origins" — the SQL clause is suppressed entirely.
func TestSearchOptions_IncludeOrigins_Empty(t *testing.T) {
	const branch = "main"
	svc := seedMixedOriginFacts(t, branch)

	results, err := svc.Search().Search(context.Background(), branch, SearchOptions{})
	require.NoError(t, err)
	require.Equal(t,
		[]string{"kb/obs/a.md", "kb/synth/d.md", "kb/synth/e.md"},
		pathsOfResults(results))
}
