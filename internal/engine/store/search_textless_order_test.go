package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/index"
)

// The text-less Search path gives every row score 100, so its row order IS
// its ranking — and the LIMIT picks which rows survive. It had no ORDER BY,
// so both were whatever SQLite's plan produced (the I6 storytest flake,
// TestSearch_RankingStableUnderDeterministicEmbedder). It now orders the way
// every other recency listing does: newest commit first, path ascending
// within a commit — the same tie-break recentFactsSearch applies after score.
//
// The newest fact has the alphabetically LAST path, and the two older ones
// share one commit, so neither insertion order nor a path-index scan yields
// the expected order by accident.
func TestSearch_TextlessOrderIsNewestFirstThenPath(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	ctx := context.Background()
	const branch = "main"
	require.NoError(t, svc.InitRepo(ctx, map[string]string{}, branch))

	_, _, err = svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{
		"kb/m.md": testFactBody("older m", 0.7, nil),
		"kb/c.md": testFactBody("older c", 0.7, nil),
	}, nil, "older pair", "")
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond) // committed_at has one-second resolution
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/z.md", testFactBody("newest z", 0.7, nil), "newest", "")
	require.NoError(t, err)

	paths := func(rs []index.SearchResult) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.Path
		}
		return out
	}

	all, err := svc.Search().Search(ctx, branch, index.SearchOptions{Path: "kb/", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/z.md", "kb/c.md", "kb/m.md"}, paths(all),
		"text-less search: newest commit first, then path ascending within a commit")

	// The LIMIT keeps the head of that order, not an arbitrary subset.
	top, err := svc.Search().Search(ctx, branch, index.SearchOptions{Path: "kb/", Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"kb/z.md", "kb/c.md"}, paths(top))
}
