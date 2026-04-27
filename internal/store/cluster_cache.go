package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ClusterCacheStore is the interface for reading and writing cached Louvain
// cluster results. The actual cache decision logic (cold/stale/fresh,
// singleflight, async refresh) lives in internal/clustercache; this layer
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
