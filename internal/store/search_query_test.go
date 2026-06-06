package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rankedEmbedder maps each fact's embedding text and the query to vectors
// chosen so that the query's cosine similarity to "match" is strictly higher
// than to "near" or "far". Padded to 768 dims so vec0 accepts it. The
// content-based dispatch keys off marker substrings ("match-target", etc.)
// that appear in both the fact title and the query text, so document and
// query roles route to the same vector.
const rankedEmbedderDim = 768

type rankedEmbedder struct{}

func (e *rankedEmbedder) vectorFor(text string) []float32 {
	out := make([]float32, rankedEmbedderDim)
	switch {
	case containsAll(text, "match-target"):
		out[0] = 1.0
	case containsAll(text, "near-target"):
		out[0] = 0.7
		out[1] = 0.3
	case containsAll(text, "far-target"):
		out[1] = 1.0
	default:
		// Query "match-target ..." falls under the match-target case above;
		// anything else (unexpected embeddings) is orthogonal to the target.
		out[2] = 1.0
	}
	return out
}

func (e *rankedEmbedder) EmbedQuery(text string) ([]float32, error) {
	return e.vectorFor(text), nil
}

func (e *rankedEmbedder) EmbedDocument(title, body string) ([]float32, error) {
	return e.vectorFor(title + " " + body), nil
}

func (e *rankedEmbedder) Dim() int { return rankedEmbedderDim }

func (e *rankedEmbedder) ID() string { return "ranked" }

func (e *rankedEmbedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vectorFor(titles[i] + " " + bodies[i])
	}
	return out, nil
}

func containsAll(haystack string, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestRecentFacts_WithQuery_SortsByRelevanceNotDate covers the bug where
// searching in "recent" mode returned matches sorted purely by committed_at,
// so a fact whose title perfectly matched the query but whose commit was
// older landed below less-relevant but more-recently-committed facts. With
// a query present, results must be ranked by relevance score; without a
// query, the existing committed_at ordering still applies.
func TestRecentFacts_WithQuery_SortsByRelevanceNotDate(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&rankedEmbedder{})

	ctx := context.Background()
	branch := "main"

	// Write three facts. The first (oldest commit) is the strongest match
	// for the query; the last (most recent commit) is the weakest. The
	// pre-fix implementation ordered the result by committed_at DESC, so
	// "far" came first and "match" last.
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/a.md", testFactBody("match-target alpha", 0.9, nil), "init a", "")
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond) // ensure distinct committed_at seconds
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/b.md", testFactBody("near-target beta", 0.8, nil), "init b", "")
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/c.md", testFactBody("far-target gamma", 0.7, nil), "init c", "")
	require.NoError(t, err)

	entries, total, err := svc.Search().RecentFacts(ctx, branch, SearchOptions{Text: "match-target alpha", Limit: 50})
	require.NoError(t, err)
	require.GreaterOrEqual(t, total, 1, "match-target fact must be in the result set")
	require.NotEmpty(t, entries)
	require.Equal(t, "kb/a.md", entries[0].Path,
		"strongest match must come first regardless of commit time (got order: %v)", pathsOf(entries))
}

func pathsOf(entries []RecentFactEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

// TestRecentFacts_PopulatesDomainAndEntities regresses the production gap
// where RecentFacts returned entries without the domain/entities JSON columns
// populated. The bridge's SessionStart hook filters by these fields, so an
// empty round-trip silently dropped every principle from the rendered prompt.
// Both code paths (no-text and text-search) must surface the slices.
func TestRecentFacts_PopulatesDomainAndEntities(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&rankedEmbedder{})

	ctx := context.Background()
	const branch = "main"
	const path = "kb/policy/match-target-tls.md"

	_, err = svc.Facts().WriteFact(ctx, branch, path,
		pragmaticFactBody("match-target Use TLS", "All cross-host traffic must use TLS 1.3+.",
			[]string{"global"}, []string{"designer"}),
		"add policy", "")
	require.NoError(t, err)

	// Non-search path: empty Text → executes the plain RecentFacts SQL.
	entries, total, err := svc.Search().RecentFacts(ctx, branch, SearchOptions{Limit: 50})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, entries, 1)
	require.Equal(t, path, entries[0].Path)
	require.Equal(t, []string{"global"}, entries[0].Domain, "Domain must round-trip via non-search RecentFacts")
	require.Equal(t, []string{"designer"}, entries[0].Entities, "Entities must round-trip via non-search RecentFacts")

	// Search path: Text != "" → executes recentFactsSearch. Use the
	// rankedEmbedder fixture so the vector search yields a hit on this fact.
	searchEntries, _, err := svc.Search().RecentFacts(ctx, branch, SearchOptions{Text: "match-target alpha", Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, searchEntries, "rankedEmbedder must match the match-target fact")
	require.Equal(t, path, searchEntries[0].Path)
	require.Equal(t, []string{"global"}, searchEntries[0].Domain, "Domain must round-trip via search RecentFacts")
	require.Equal(t, []string{"designer"}, searchEntries[0].Entities, "Entities must round-trip via search RecentFacts")
}
