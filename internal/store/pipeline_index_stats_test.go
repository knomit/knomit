package store

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests for the session stat counters. They exist because the synthesis engine
// is per-call stateless — a fresh Reviewer is built for every continue call —
// so the running totals of what a session changed have nowhere to live except
// the session row.

// TestAddPipelineSessionStats_Accumulates pins the contract that matters to the
// caller: repeated calls add up rather than overwrite, because each applied
// work item contributes only its own delta.
func TestAddPipelineSessionStats_Accumulates(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)

	got, err := pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, PipelineSessionStats{}, got.Stats, "a new session starts at zero")

	require.NoError(t, pi.AddPipelineSessionStats(ctx, sess.ID,
		PipelineSessionStats{Pruned: 2, Merged: 1}))
	require.NoError(t, pi.AddPipelineSessionStats(ctx, sess.ID,
		PipelineSessionStats{Updated: 3, Synthesized: 4, Pruned: 1}))

	got, err = pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, PipelineSessionStats{Pruned: 3, Merged: 1, Updated: 3, Synthesized: 4},
		got.Stats, "each call must add its delta to the running total")
}

// TestAddPipelineSessionStats_ConcurrentAddsAllLand is why the addition happens
// in SQL rather than as a read-modify-write in Go: concurrent appliers of
// different work items would otherwise clobber each other's contributions.
func TestAddPipelineSessionStats_ConcurrentAddsAllLand(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()
	ctx := context.Background()

	sess, err := pi.CreatePipelineSession(ctx, "review", "agent/test")
	require.NoError(t, err)

	const callers = 8
	var wg sync.WaitGroup
	wg.Add(callers)
	errs := make([]error, callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			errs[i] = pi.AddPipelineSessionStats(ctx, sess.ID, PipelineSessionStats{Synthesized: 1})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "caller %d errored", i)
	}

	got, err := pi.GetPipelineSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, callers, got.Stats.Synthesized, "no concurrent contribution may be lost")
}
