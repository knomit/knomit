// Package clustercache caches Louvain community detection results so the
// review tool does not recompute clusters on every call. Wraps
// store.SearchIndex.ClusterFacts with an SQLite-backed cache (table
// cluster_cache, see migration 000004), a singleflight to dedup concurrent
// recomputes, and a background checker that refreshes stale entries during
// quiet periods (when the latest commit on a branch is older than
// QuietThreshold).
package clustercache

import (
	"fmt"
	"time"

	"knomit/internal/config"
)

// Config holds runtime parameters for the cache. Built once at app startup
// from config.ClusterCacheConfig (which holds the raw TOML/env strings) by
// ConfigFrom. CheckInterval == 0 disables the background checker.
type Config struct {
	QuietThreshold time.Duration
	CheckInterval  time.Duration
	MaxConcurrent  int
}

// ConfigFrom parses the raw config.ClusterCacheConfig durations into a
// runtime Config. Returns an error rather than silently substituting
// defaults so misconfigurations surface at boot rather than at first review.
func ConfigFrom(raw config.ClusterCacheConfig) (Config, error) {
	q, err := parseDur("quiet_threshold", raw.QuietThreshold, 10*time.Second)
	if err != nil {
		return Config{}, err
	}
	c, err := parseDur("check_interval", raw.CheckInterval, 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	maxC := raw.MaxConcurrent
	if maxC <= 0 {
		maxC = 1
	}
	return Config{
		QuietThreshold: q,
		CheckInterval:  c,
		MaxConcurrent:  maxC,
	}, nil
}

// parseDur returns the parsed duration, or the default if s is empty.
// "0" / "0s" yields a zero duration (which disables the relevant feature).
func parseDur(field, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("cluster_cache.%s: %w", field, err)
	}
	return d, nil
}
