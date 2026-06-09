package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestComputeAndCacheClusters_CallerCtxCanceled regresses the singleflight
// poisoning bug where the closure passed to clusterSF.Do captured the first
// caller's request-scoped ctx. If that caller's ctx cancelled, every SQL op
// inside the closure (HeadCommit lookup, branch_facts probe, louvain query,
// cluster_cache upsert) errored with context.Canceled, the cache row was
// never written, and every other waiter — including the background checker
// passing a fresh 5-minute ctx — got the same cancellation back. Worst case
// under load: the cache never warmed.
//
// Fix: the closure detaches from the caller's ctx (context.WithoutCancel +
// generous timeout) so a single hostile/cancelled caller cannot poison the
// shared compute. The caller-side select on ctx.Done() lets cancelled callers
// return promptly while the detached compute continues for everyone else.
//
// The test asserts the post-fix invariant: a caller with a pre-cancelled ctx
// must still result in the cache being populated. The caller may legally
// return ctx.Err() (DoChan + select path) or succeed; what matters is the
// cache row eventually exists so the next caller sees a hit.
func TestComputeAndCacheClusters_CallerCtxCanceled_StillPopulatesCache(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	// At least one fact so ClusterFacts doesn't short-circuit on the empty
	// branch fast-path (which would bypass the SQL ops where ctx-cancellation
	// surfaces and let the buggy closure complete despite the cancelled ctx).
	writeClusterTestFact(t, svc, "main", "kb/a.md", "a", "alpha body")
	writeClusterTestFact(t, svc, "main", "kb/b.md", "b", "beta body")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Caller may return ctx.Err() (post-fix DoChan + select) or success
	// (minimal-fix variant). Either is acceptable; we don't assert on it.
	_, _ = svc.si.computeAndCacheClusters(ctx, "main", 1.0, 2)

	cacheStore := &clusterCacheStore{rh: svc.rh}
	require.Eventually(t, func() bool {
		_, found, err := cacheStore.Get(context.Background(), "main", 1.0, 2)
		return err == nil && found
	}, 5*time.Second, 25*time.Millisecond,
		"cluster cache must populate even when the calling ctx is cancelled — "+
			"otherwise a single timed-out request poisons the shared compute")
}

func writeClusterTestFact(t *testing.T, svc *Service, branch, path, title, body string) string {
	t.Helper()
	content := "---\ntype: observation\n---\n# " + title + "\n\n" + body + "\n"
	res, err := svc.Facts().WriteFact(context.Background(), branch, path, content, "test", "test")
	require.NoError(t, err)
	return res.CommitHash
}

// TestComputeAndCacheClusters_CommitDuringCompute regresses the cache-key
// drift bug: the head_commit stored on the cache row was captured *before*
// ClusterFacts ran, so a commit arriving during the compute would stamp the
// cache with the pre-compute HEAD. The next request would see current HEAD
// ≠ cached → mark stale → fire another async refresh that races against
// any further activity, and the cache would never converge to "fresh"
// while writes kept arriving.
//
// Fix: capture HEAD *after* ClusterFacts returns, so the cache row reflects
// the latest commit observed by the index at the moment the compute
// finished — including any that landed during it.
//
// The test injects a commit deterministically between ClusterFacts and the
// HEAD capture using clusterCachePostComputeHook. After the compute, the
// cache row's head_commit must equal the *post-hook* commit hash, not the
// HEAD that existed before compute started.
func TestComputeAndCacheClusters_CommitDuringCompute_CacheTracksLatestCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	writeClusterTestFact(t, svc, "main", "kb/a.md", "a", "alpha body")

	var commitDuringCompute string
	cleanup := SetClusterCachePostComputeHookForTest(func() {
		commitDuringCompute = writeClusterTestFact(t, svc, "main", "kb/b.md", "b", "beta body")
	})
	t.Cleanup(cleanup)

	_, err = svc.si.computeAndCacheClusters(context.Background(), "main", 1.0, 2)
	require.NoError(t, err)
	require.NotEmpty(t, commitDuringCompute, "hook must have written the during-compute commit")

	cacheStore := &clusterCacheStore{rh: svc.rh}
	row, found, err := cacheStore.Get(context.Background(), "main", 1.0, 2)
	require.NoError(t, err)
	require.True(t, found, "cache row must exist after compute")
	require.Equal(t, commitDuringCompute, row.HeadCommit,
		"cache key must reflect the latest commit observed by the index, "+
			"including ones that landed during ClusterFacts — otherwise active "+
			"branches loop refreshing forever and never converge to fresh")
}
