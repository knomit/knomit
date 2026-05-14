package store

// clusterCachePostComputeHook is invoked inside computeAndCacheClusters
// after ClusterFacts returns but before the watermark commit is captured.
// nil in production. Used by tests to deterministically simulate a commit
// landing during the compute, which exercises the path where buggy code
// would have written a stale watermark to the cache.
var clusterCachePostComputeHook func()

// SetClusterCachePostComputeHookForTest installs fn (set to nil to clear)
// and returns a cleanup function restoring the previous value. Only used
// from tests; not part of the public API.
func SetClusterCachePostComputeHookForTest(fn func()) func() {
	old := clusterCachePostComputeHook
	clusterCachePostComputeHook = fn
	return func() { clusterCachePostComputeHook = old }
}
