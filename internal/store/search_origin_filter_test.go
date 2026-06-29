package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestCompletions_Origin pins that origin autocomplete returns the three valid
// origin values so the UI's Origin filter category has suggestions.
func TestCompletions_Origin(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()

	got, err := svc.Search().Completions(ctx, "main", "origin", "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{"authored", "distilled", "discovered"}, got)
}

// TestRecentFacts_IncludeOriginsFilters pins that SearchOptions.IncludeOrigins
// restricts the facts collection to the requested origins — the filter the web
// UI's Origin chip relies on.
func TestRecentFacts_IncludeOriginsFilters(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	branch := "main"

	write := func(path, title string, origin fact.Origin) {
		f := fact.NewFact("placeholder.md")
		f.Title = title
		f.Confidence = 0.9
		f.Sources = 1
		f.Domain = []string{"x"}
		f.Entities = []string{"y"}
		f.Type = fact.Observation
		f.Origin = origin
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = svc.Facts().WriteFact(ctx, branch, path, out, "init", "")
		require.NoError(t, err)
	}
	write("kb/a.md", "authored fact", fact.Authored)
	write("kb/b.md", "discovered fact", fact.Discovered)
	write("kb/c.md", "distilled fact", fact.Distilled)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	// Filtering to discovered returns only the discovered fact.
	got, total, err := svc.Search().RecentFacts(ctx, branch, SearchOptions{
		IncludeOrigins: []string{"discovered"},
		Limit:          50,
	})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, got, 1)
	require.Equal(t, "kb/b.md", got[0].Path)

	// Filtering to two origins returns both, but not the third.
	got, total, err = svc.Search().RecentFacts(ctx, branch, SearchOptions{
		IncludeOrigins: []string{"discovered", "distilled"},
		Limit:          50,
	})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	paths := []string{got[0].Path, got[1].Path}
	require.ElementsMatch(t, []string{"kb/b.md", "kb/c.md"}, paths)

	// No origin filter returns all three.
	_, total, err = svc.Search().RecentFacts(ctx, branch, SearchOptions{Limit: 50})
	require.NoError(t, err)
	require.Equal(t, 3, total)
}
