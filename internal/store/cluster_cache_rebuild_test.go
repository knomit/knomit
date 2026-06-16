package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestClusterCache_SkipsComputeWhileRebuilding regresses the wasteful contention
// where the 5s background cluster-checker computed clusters on a half-built
// index during a rebuild — failing to write with "database is locked" every
// tick. CachedClusterFacts must skip compute/write while a rebuild is active and
// resume once it clears.
func TestClusterCache_SkipsComputeWhileRebuilding(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	svc.SetEmbedder(&stub768Embedder{})

	ctx := context.Background()
	f := fact.NewFact("placeholder.md")
	f.Title = "Alpha"
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"ai-governance"}
	f.Entities = []string{"x"}
	f.Type = fact.Observation
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/a.md", out, "init", "")
	require.NoError(t, err)

	si := svc.Search().(*searchIndex)
	countRows := func() int {
		var n int
		require.NoError(t, si.rh.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cluster_cache`).Scan(&n))
		return n
	}
	require.Zero(t, countRows(), "cold cache: no rows yet")

	// While rebuilding, a cold compute must be skipped — no row written.
	si.rebuilding.Store(true)
	_, err = si.CachedClusterFacts(ctx, "main", 2.0, 2)
	require.NoError(t, err)
	require.Zero(t, countRows(), "cluster cache must NOT be computed/written during a rebuild")

	// Once the rebuild clears, the next call computes and persists.
	si.rebuilding.Store(false)
	_, err = si.CachedClusterFacts(ctx, "main", 2.0, 2)
	require.NoError(t, err)
	require.Equal(t, 1, countRows(), "cluster cache must compute after the rebuild completes")
}
