package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
)

// ClusterCacheStore is the interface for reading and writing cached Louvain
// cluster results. The cache decision logic (cold/stale/fresh, singleflight,
// async refresh) lives on *searchIndex via CachedClusterFacts; this layer
// only handles SQL CRUD on the cluster_cache table.
type ClusterCacheStore interface {
	Get(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterCacheRow, bool, error)
	Put(ctx context.Context, branch string, resolution float64, minCommunitySize int, headCommit string, result ClusterResult) error
}

// ClusterCacheRow is a cache hit returned by ClusterCacheStore.Get.
type ClusterCacheRow struct {
	HeadCommit string
	Result     ClusterResult
	ComputedAt time.Time
}

// clusterCacheStore is the concrete ClusterCacheStore. It shares the
// repoHandler's *sql.DB and branch-name → branch-id resolution.
type clusterCacheStore struct {
	rh *repoHandler
}

// Compile-time assertion.
var _ ClusterCacheStore = (*clusterCacheStore)(nil)

// Get returns the cached cluster result for the given key, if any. The
// caller is responsible for comparing HeadCommit against the branch's
// current HEAD to decide whether the cache is stale.
func (s *clusterCacheStore) Get(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterCacheRow, bool, error) {
	branchID, err := s.rh.branchID(ctx, branch)
	if err != nil {
		return ClusterCacheRow{}, false, fmt.Errorf("cluster cache get: %w", err)
	}

	var (
		headCommit    string
		clustersJSON  string
		noiseJSON     string
		computedAtSec int64
	)
	row := conn(ctx, s.rh.db).QueryRowContext(ctx, `
		SELECT head_commit, clusters_json, noise_json, computed_at
		FROM cluster_cache
		WHERE branch_id = ? AND resolution = ? AND min_community_size = ?
	`, branchID, resolution, minCommunitySize)
	err = row.Scan(&headCommit, &clustersJSON, &noiseJSON, &computedAtSec)
	if err == sql.ErrNoRows {
		return ClusterCacheRow{}, false, nil
	}
	if err != nil {
		return ClusterCacheRow{}, false, fmt.Errorf("cluster cache scan: %w", err)
	}

	var clusters map[int][]string
	if err := json.Unmarshal([]byte(clustersJSON), &clusters); err != nil {
		return ClusterCacheRow{}, false, fmt.Errorf("cluster cache decode clusters: %w", err)
	}
	var noise []string
	if err := json.Unmarshal([]byte(noiseJSON), &noise); err != nil {
		return ClusterCacheRow{}, false, fmt.Errorf("cluster cache decode noise: %w", err)
	}

	return ClusterCacheRow{
		HeadCommit: headCommit,
		Result:     ClusterResult{Clusters: clusters, Noise: noise},
		ComputedAt: time.Unix(computedAtSec, 0),
	}, true, nil
}

// Put writes (or replaces) the cached cluster result for the given key.
func (s *clusterCacheStore) Put(ctx context.Context, branch string, resolution float64, minCommunitySize int, headCommit string, result ClusterResult) error {
	branchID, err := s.rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("cluster cache put: %w", err)
	}

	clusters := result.Clusters
	if clusters == nil {
		clusters = map[int][]string{}
	}
	noise := result.Noise
	if noise == nil {
		noise = []string{}
	}
	clustersJSON, err := json.Marshal(clusters)
	if err != nil {
		return fmt.Errorf("cluster cache encode clusters: %w", err)
	}
	noiseJSON, err := json.Marshal(noise)
	if err != nil {
		return fmt.Errorf("cluster cache encode noise: %w", err)
	}

	_, err = conn(ctx, s.rh.db).ExecContext(ctx, `
		INSERT INTO cluster_cache(branch_id, resolution, min_community_size, head_commit, clusters_json, noise_json, computed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(branch_id, resolution, min_community_size) DO UPDATE SET
			head_commit = excluded.head_commit,
			clusters_json = excluded.clusters_json,
			noise_json = excluded.noise_json,
			computed_at = excluded.computed_at
	`, branchID, resolution, minCommunitySize, headCommit, string(clustersJSON), string(noiseJSON), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cluster cache upsert: %w", err)
	}
	return nil
}

// CachedClusterFacts is the cache-aware entry point preferred by the review
// pipeline. The decision tree:
//
//   - Cold cache (no row): compute synchronously, write, return. The
//     background checker (repos.Manager.StartClusterChecker) is supposed to
//     warm cold caches before any caller asks; this branch is the safety
//     net, not the steady state.
//   - Fresh cache (row HEAD == current HEAD): return cached.
//   - Stale cache (row HEAD differs): return cached value immediately and
//     fire an asynchronous refresh whose lifetime is decoupled from the
//     request context. Next caller benefits.
//
// Concurrent calls to this method (e.g. a review + the checker, or two
// reviews) for the same (branch, resolution, minCommunitySize) collapse via
// the per-searchIndex singleflight group so Louvain runs at most once per
// key at any moment.
func (si *searchIndex) CachedClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error) {
	headCommit, err := si.rh.HeadCommit(ctx, branch)
	if err != nil {
		return ClusterResult{}, fmt.Errorf("CachedClusterFacts head: %w", err)
	}
	cacheStore := &clusterCacheStore{rh: si.rh}
	row, found, err := cacheStore.Get(ctx, branch, resolution, minCommunitySize)
	if err != nil {
		return ClusterResult{}, fmt.Errorf("CachedClusterFacts get: %w", err)
	}

	if !found {
		log.Info().Str("branch", branch).Msg("cluster cache: cold, computing synchronously")
		// Mark in flight so the background checker doesn't re-dispatch and
		// re-log every tick during a slow cold compute. Best-effort: if a
		// refresh for this key is already marked we still compute (joining the
		// singleflight to get a result — there is no cached row to return), and
		// only the acquirer clears the marker.
		key := clusterCacheKey(branch, resolution, minCommunitySize)
		if _, inFlight := si.clusterRefreshing.LoadOrStore(key, struct{}{}); !inFlight {
			defer si.clusterRefreshing.Delete(key)
		}
		return si.computeAndCacheClusters(ctx, branch, resolution, minCommunitySize)
	}

	if row.HeadCommit != headCommit {
		// Log only when this call actually starts the refresh. If one is
		// already in flight for this key (a long Louvain compute leaves the
		// row stale for its whole duration), refreshClustersAsync is a no-op
		// and we stay quiet — otherwise every read re-logs every tick.
		if si.refreshClustersAsync(branch, resolution, minCommunitySize) {
			log.Debug().
				Str("branch", branch).
				Str("cached_head", shortHash(row.HeadCommit)).
				Str("current_head", shortHash(headCommit)).
				Msg("cluster cache: stale, returning cached + async refresh")
		}
	}
	return row.Result, nil
}

// clusterCacheKey is the canonical identity for a cluster-cache entry,
// shared by the singleflight group and the in-flight refresh marker so both
// collapse on exactly the same (branch, resolution, minCommunitySize) tuple.
func clusterCacheKey(branch string, resolution float64, minCommunitySize int) string {
	return fmt.Sprintf("%s|%g|%d", branch, resolution, minCommunitySize)
}

// ClusterRefreshInFlight reports whether a cluster-cache refresh for the given
// key is currently running (async stale refresh or synchronous cold compute).
// The background cluster checker consults this to avoid re-dispatching (and
// re-logging "triggering refresh") every tick while a long Louvain compute is
// already in flight for the same key.
func (si *searchIndex) ClusterRefreshInFlight(branch string, resolution float64, minCommunitySize int) bool {
	_, running := si.clusterRefreshing.Load(clusterCacheKey(branch, resolution, minCommunitySize))
	return running
}

// computeAndCacheClusters runs ClusterFacts under the singleflight group,
// writes the result to the cache table, and returns it. Used by the
// cold-cache sync path and by refreshClustersAsync.
//
// Two ctx-related properties matter here:
//
//   - The singleflight closure is detached from the calling ctx (via
//     WithoutCancel + a 5-minute timeout). The result is shared with every
//     other waiter — concurrent reviewers, the background checker — so a
//     single cancelled caller must not poison the compute and leave the
//     cache cold for everyone else.
//   - DoChan + select on ctx.Done lets a cancelled caller return promptly
//     while the detached compute continues in the singleflight goroutine
//     and populates the cache for the next request.
//
// The cache watermark (head_commit column) is captured *after* ClusterFacts
// returns. Capturing before would let a commit arriving during the compute
// stamp the cache with the pre-compute HEAD, leaving the next caller's
// freshness check permanently mismatched against the current HEAD on active
// branches and triggering an async-refresh loop that never converges to
// "fresh" while writes keep arriving. The git ref is the canonical source
// of "latest commit on branch": sequential commits advance it atomically,
// without the second-resolution ties that affect commit_log timestamps.
func (si *searchIndex) computeAndCacheClusters(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error) {
	key := clusterCacheKey(branch, resolution, minCommunitySize)
	ch := si.clusterSF.DoChan(key, func() (any, error) {
		workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()

		start := time.Now()
		result, err := si.ClusterFacts(workCtx, branch, resolution, minCommunitySize)
		if err != nil {
			return ClusterResult{}, err
		}
		if hook := clusterCachePostComputeHook; hook != nil {
			hook()
		}
		headCommit, err := si.rh.HeadCommit(workCtx, branch)
		if err != nil {
			return ClusterResult{}, fmt.Errorf("compute head: %w", err)
		}
		cacheStore := &clusterCacheStore{rh: si.rh}
		if err := cacheStore.Put(workCtx, branch, resolution, minCommunitySize, headCommit, result); err != nil {
			return ClusterResult{}, fmt.Errorf("compute put: %w", err)
		}
		log.Info().
			Str("branch", branch).
			Str("head", shortHash(headCommit)).
			Int("clusters", len(result.Clusters)).
			Int("noise", len(result.Noise)).
			Dur("elapsed", time.Since(start)).
			Msg("cluster cache: refreshed")
		return result, nil
	})
	select {
	case <-ctx.Done():
		return ClusterResult{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return ClusterResult{}, res.Err
		}
		return res.Val.(ClusterResult), nil
	}
}

// refreshClustersAsync fires a background recompute that outlives the
// request context, and reports whether it actually started one. If a refresh
// for this key is already in flight it returns false without launching a
// second goroutine: the singleflight would collapse the work anyway, but the
// in-flight marker additionally lets callers skip the redundant dispatch and
// log. The marker is held for the goroutine's whole lifetime (compute
// included), so it covers a multi-second Louvain run, not just the launch.
//
// The 5-minute ceiling matches the order of magnitude of a worst-case Louvain
// on a very large knowledge base; without it a hung query could pin a
// goroutine indefinitely. Errors are logged, not surfaced — the worst case is
// the cache stays stale until the next trigger.
func (si *searchIndex) refreshClustersAsync(branch string, resolution float64, minCommunitySize int) (started bool) {
	key := clusterCacheKey(branch, resolution, minCommunitySize)
	// Atomic check-and-claim: LoadOrStore reports whether the key was already
	// present. If so, a refresh is already in flight — skip without launching a
	// second goroutine.
	if _, inFlight := si.clusterRefreshing.LoadOrStore(key, struct{}{}); inFlight {
		return false
	}

	go func() {
		defer si.clusterRefreshing.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := si.computeAndCacheClusters(ctx, branch, resolution, minCommunitySize); err != nil {
			log.Warn().Err(err).Str("branch", branch).Msg("cluster cache: async refresh failed")
		}
	}()
	return true
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
