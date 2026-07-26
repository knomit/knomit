package repos

import (
	"fmt"
	"time"
)

// defaultClusterResolution / defaultClusterMinCommunitySize are the canonical
// fallbacks when config leaves the Louvain knobs unset. Kept here as the single
// source of truth shared by the RepoInstance builder and the synthesize read
// path (via RepoInstance.ClusterResolution/ClusterMinCommunitySize), so scoped
// clustering always runs with the configured granularity.
const (
	// defaultClusterResolution is the Louvain γ for scoped review clustering.
	// Calibrated to 4.0 so the SIMILAR_TO-only subgraph (denser than the old
	// multi-edge-type global graph, and clustered by gonum rather than
	// the property graph) yields review-sized communities (~35 of ~20-50 facts),
	// matching the granularity the prior global-Louvain review produced.
	defaultClusterResolution       = 4.0
	defaultClusterMinCommunitySize = 2
)

// clusterResolutionOrDefault / clusterMinCommunityOrDefault apply the canonical
// fallbacks for unset (<=0) config values, used by the RepoInstance builder.
func clusterResolutionOrDefault(v float64) float64 {
	if v <= 0 {
		return defaultClusterResolution
	}
	return v
}

func clusterMinCommunityOrDefault(v int) int {
	if v <= 0 {
		return defaultClusterMinCommunitySize
	}
	return v
}

// parseConfigDur parses a raw TOML/env duration string for the named config
// section/field. An empty string yields def; a malformed value is an error
// wrapped as "<section>.<field>: ...". A parsed "0"/"0s" is returned as-is
// (callers decide whether zero disables a loop or should be clamped). Shared by
// the session reaper config.
func parseConfigDur(section, field, s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s.%s: %w", section, field, err)
	}
	return d, nil
}
