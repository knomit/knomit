package clustercache

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/singleflight"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// Cache is the cluster-cache facade. One instance per knomit process,
// shared across all repos. Per-repo storage lives in each repo's SQLite
// cluster_cache table.
//
// Read path (Get):
//   - Cold cache  → synchronous compute, write, return.
//   - Stale cache (HEAD differs from cache row) → return cached value
//     immediately and fire an async refresh that outlives the request ctx.
//   - Fresh cache → return cached value.
//
// Concurrent recomputes for the same key are deduped via singleflight; the
// global semaphore (sem) caps the total in-flight recompute count across
// all repos and branches.
type Cache struct {
	cfg Config
	sf  singleflight.Group
	sem chan struct{}
}

// ClusterFn matches store.SearchIndex.ClusterFacts. The Cache wraps it so
// callers (synthesize.Reviewer) can use a cache-backed function without
// knowing whether the result came from the cache or a fresh compute.
type ClusterFn func(ctx context.Context, branch string, resolution float64, minCommunitySize int) (store.ClusterResult, error)

// New constructs a Cache with the parsed runtime config. MaxConcurrent
// caps total concurrent recomputes across all repos+branches; values <= 0
// are normalised to 1 in ConfigFrom.
func New(cfg Config) *Cache {
	return &Cache{
		cfg: cfg,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
}

// ClusterFnFor returns a cache-backed ClusterFn bound to a specific repo.
// This is what the Reviewer/ScopedCluster receive in place of the raw
// idx.ClusterFacts call.
func (c *Cache) ClusterFnFor(ri *repos.RepoInstance) ClusterFn {
	return func(ctx context.Context, branch string, resolution float64, minCommunitySize int) (store.ClusterResult, error) {
		return c.Get(ctx, ri, branch, resolution, minCommunitySize)
	}
}

// Get implements the read path described on Cache. ri must be non-nil.
func (c *Cache) Get(ctx context.Context, ri *repos.RepoInstance, branch string, resolution float64, minCommunitySize int) (store.ClusterResult, error) {
	cached, headCommit, found, err := c.lookup(ctx, ri, branch, resolution, minCommunitySize)
	if err != nil {
		return store.ClusterResult{}, err
	}

	if !found {
		// Cold cache — must compute synchronously, otherwise we have nothing
		// to return. This is the slow path that the background checker is
		// designed to avoid.
		log.Info().Str("branch", branch).Msg("cluster cache: cold, computing synchronously")
		return c.compute(ctx, ri, branch, resolution, minCommunitySize)
	}

	// Cache hit. If HEAD has advanced since cache was written, kick off an
	// async refresh and return the stale value.
	if cached.HeadCommit != headCommit {
		log.Debug().
			Str("branch", branch).
			Str("cached_head", shortHash(cached.HeadCommit)).
			Str("current_head", shortHash(headCommit)).
			Msg("cluster cache: stale, returning cached + async refresh")
		c.refreshAsync(ri, branch, resolution, minCommunitySize)
	}
	return cached.Result, nil
}

// lookup returns the cached row plus the branch's current HEAD so the
// caller can detect staleness. found=false means there's no cache row yet
// (cold).
func (c *Cache) lookup(ctx context.Context, ri *repos.RepoInstance, branch string, resolution float64, minCommunitySize int) (store.ClusterCacheRow, string, bool, error) {
	var (
		row        store.ClusterCacheRow
		headCommit string
		found      bool
		err        error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = fmt.Errorf("cluster cache: store unavailable")
			return
		}
		headCommit, err = svc.Branches().HeadCommit(ctx, branch)
		if err != nil {
			return
		}
		row, found, err = svc.ClusterCache().Get(ctx, branch, resolution, minCommunitySize)
	})
	return row, headCommit, found, err
}

// compute runs ClusterFacts under the read lock, writes the result to the
// cache, and returns it. Used both for cold-cache reads (synchronous) and
// for refreshes (called from refreshAsync and from the checker, with a
// detached ctx).
func (c *Cache) compute(ctx context.Context, ri *repos.RepoInstance, branch string, resolution float64, minCommunitySize int) (store.ClusterResult, error) {
	key := sfKey(ri.Name(), branch, resolution, minCommunitySize)
	v, err, _ := c.sf.Do(key, func() (any, error) {
		// Acquire a slot from the global concurrency semaphore. ctx
		// cancellation breaks out cleanly so request-scoped computes still
		// honor client cancellation; checker computes pass a detached ctx.
		select {
		case c.sem <- struct{}{}:
		case <-ctx.Done():
			return store.ClusterResult{}, ctx.Err()
		}
		defer func() { <-c.sem }()

		var (
			result     store.ClusterResult
			headCommit string
			err        error
		)
		start := time.Now()
		ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				err = fmt.Errorf("cluster cache: store unavailable")
				return
			}
			headCommit, err = svc.Branches().HeadCommit(ctx, branch)
			if err != nil {
				return
			}
			result, err = svc.Search().ClusterFacts(ctx, branch, resolution, minCommunitySize)
			if err != nil {
				return
			}
			err = svc.ClusterCache().Put(ctx, branch, resolution, minCommunitySize, headCommit, result)
		})
		if err != nil {
			return store.ClusterResult{}, err
		}
		log.Info().
			Str("repo", ri.Name()).
			Str("branch", branch).
			Str("head", shortHash(headCommit)).
			Int("clusters", len(result.Clusters)).
			Int("noise", len(result.Noise)).
			Dur("elapsed", time.Since(start)).
			Msg("cluster cache: refreshed")
		return result, nil
	})
	if err != nil {
		return store.ClusterResult{}, err
	}
	return v.(store.ClusterResult), nil
}

// refreshAsync fires a background compute that outlives the request ctx.
// Used both by the read path on stale-cache hits and by the checker.
// Errors are logged but not surfaced — the worst case is the cache stays
// stale until the next trigger.
func (c *Cache) refreshAsync(ri *repos.RepoInstance, branch string, resolution float64, minCommunitySize int) {
	go func() {
		// 5-minute ceiling so a hung Louvain query can't pin a refresh
		// goroutine forever; matches the order-of-magnitude of a worst-case
		// recompute on a very large knowledge base.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := c.compute(ctx, ri, branch, resolution, minCommunitySize); err != nil {
			log.Warn().Err(err).Str("repo", ri.Name()).Str("branch", branch).Msg("cluster cache: async refresh failed")
		}
	}()
}

// sfKey builds the singleflight key. Includes repo name to keep concurrent
// reads on different repos from collapsing onto each other's compute.
func sfKey(repo, branch string, resolution float64, minCommunitySize int) string {
	return fmt.Sprintf("%s|%s|%g|%d", repo, branch, resolution, minCommunitySize)
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
