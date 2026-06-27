package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// openHypothesizeTestStore opens a fresh store, initialises the agent branch,
// and returns a (Service, RepoInstance, mcpStore) triple ready for hypothesize
// tests. No embedder: search paths exercised here are SQL-only.
func openHypothesizeTestStore(t *testing.T) (*store.Service, *repos.RepoInstance, mcpStore) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})

	s := mcpStore{
		facts:    svc.Facts(),
		search:   svc.Search(),
		pipeline: svc.Pipeline(),
		branches: svc.Branches(),
	}
	return svc, ri, s
}

// TestScopedHypothesizeStart_EmptyPool_DoesNotAdvanceWatermark is the
// regression guard for the early-return watermark-poisoning bug. When
// hypothesizeStart finds zero synthesis facts and a scope filter is active,
// it must NOT advance the watermark to HEAD. Before the fix, the early return
// unconditionally called SetPipelineWatermark, permanently hiding all
// out-of-scope synthesis facts from future unscoped sessions.
func TestScopedHypothesizeStart_EmptyPool_DoesNotAdvanceWatermark(t *testing.T) {
	svc, ri, s := openHypothesizeTestStore(t)
	ctx := context.Background()
	agentBranch := "agent/test"

	// No synthesis facts exist → the early-return path is taken.
	scope := synthesize.ScopeFilter{Domain: []string{"auth"}}
	result, err := hypothesizeStart(ctx, ri, s, agentBranch, synthesize.EffortNormal, scope)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Done, "empty pool → Done immediately")

	// Watermark must NOT have advanced.
	watermark, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", agentBranch)
	require.NoError(t, err)
	require.Empty(t, watermark,
		"scoped hypothesizeStart with empty pool must not advance the watermark: "+
			"out-of-scope synthesis facts would be permanently hidden from future unscoped sessions")
}

// TestUnscopedHypothesizeStart_EmptyPool_AdvancesWatermark confirms the
// unscoped early-return path still advances the watermark (the desired
// behaviour: nothing to do, mark progress).
func TestUnscopedHypothesizeStart_EmptyPool_AdvancesWatermark(t *testing.T) {
	svc, ri, s := openHypothesizeTestStore(t)
	ctx := context.Background()
	agentBranch := "agent/test"

	result, err := hypothesizeStart(ctx, ri, s, agentBranch, synthesize.EffortNormal, synthesize.ScopeFilter{})
	require.NoError(t, err)
	require.True(t, result.Done)

	// Unscoped: watermark SHOULD advance so the next run is incremental.
	watermark, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", agentBranch)
	require.NoError(t, err)
	require.NotEmpty(t, watermark,
		"unscoped hypothesizeStart with empty pool must advance watermark for incremental next run")
}
