package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClusterResultClusterOf(t *testing.T) {
	cr := ClusterResult{Clusters: map[int][]string{0: {"a", "b"}, 1: {"c"}}, Noise: []string{"d", "e"}}
	m := cr.ClusterOf()
	if m["a"] != 0 || m["b"] != 0 || m["c"] != 1 {
		t.Fatalf("clustered ids wrong: %+v", m)
	}
	if m["d"] == m["e"] {
		t.Errorf("noise paths must get distinct ids: %+v", m)
	}
	if m["d"] >= 0 || m["e"] >= 0 {
		t.Errorf("noise ids must be negative: %+v", m)
	}
}

// TestClusterFacts_EmptyBranch_ReturnsEmpty regresses the bug where running
// ClusterFacts against a branch with no facts (e.g. immediately after
// InitRepo) caused GraphQLite's louvain() to abort the statement with
// "abort due to ROLLBACK". The cluster-cache background checker hit this
// every tick on freshly initialised repos.
func TestClusterFacts_EmptyBranch_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	result, err := svc.Search().ClusterFacts(context.Background(), "main", 1.0, 2)
	require.NoError(t, err, "ClusterFacts on empty branch must not error")
	require.Empty(t, result.Clusters, "empty branch should have no clusters")
	require.Empty(t, result.Noise, "empty branch should have no noise")
}
