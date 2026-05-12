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
// than to "near" or "far". Padded to factsVecDim so vec0 accepts it.
type rankedEmbedder struct{}

func (e *rankedEmbedder) Embed(text string) ([]float32, error) {
	out := make([]float32, factsVecDim)
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
	return out, nil
}

func (e *rankedEmbedder) EmbedBatch(texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, _ := e.Embed(t)
		out[i] = v
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

	entries, total, err := svc.Search().RecentFacts(ctx, branch, "", "match-target alpha", 50, 0, nil, nil, nil, nil, nil)
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
