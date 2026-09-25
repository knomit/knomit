package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Two starts racing for one empty slot: exactly one creates the session and
// the other is told the slot changed, so it can re-read and resume or refuse.
// Neither may surface "database is locked" — a lost race is a decision, not a
// storage failure.
func TestCreatePipelineSessionReplacing_ConcurrentStartsSerialise(t *testing.T) {
	svc := newPhaseTestService(t)
	pi := svc.Pipeline()

	for round := 0; round < 40; round++ {
		branch := fmt.Sprintf("agent/race-%d", round)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = pi.CreatePipelineSessionReplacing(context.Background(),
					"review", branch, fmt.Sprintf("caller-%d", i), "key", "")
			}(i)
		}
		close(start)
		wg.Wait()

		created, changed := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				created++
			case errors.Is(err, ErrPipelineSlotChanged):
				changed++
			default:
				t.Fatalf("round %d: a lost race must be ErrPipelineSlotChanged, got %v", round, err)
			}
		}
		require.Equal(t, 1, created, "round %d: exactly one start creates", round)
		require.Equal(t, 1, changed, "round %d: the other is told the slot changed", round)
	}
}

// A failed start abandons its own session only while it is still planning:
// once planned, another caller may have resumed it, and the abandon must be a
// no-op.
func TestAbandonPlanningPipelineSession_OnlyWhilePlanning(t *testing.T) {
	ctx := context.Background()
	pi := newPhaseTestService(t).Pipeline()

	planning, err := pi.CreatePipelineSessionReplacing(ctx, "review", "agent/a", "p", "key", "")
	require.NoError(t, err)
	require.NoError(t, pi.AbandonPlanningPipelineSession(ctx, planning.ID))
	got, err := pi.GetPipelineSession(ctx, planning.ID)
	require.NoError(t, err)
	require.Equal(t, "abandoned", got.Status, "a session still planning is abandoned")

	planned, err := pi.CreatePipelineSessionReplacing(ctx, "review", "agent/b", "p", "key", "")
	require.NoError(t, err)
	_, markErr := pi.MarkPipelineSessionPlanned(ctx, planned.ID)
	require.NoError(t, markErr)
	require.NoError(t, pi.AbandonPlanningPipelineSession(ctx, planned.ID))
	got, err = pi.GetPipelineSession(ctx, planned.ID)
	require.NoError(t, err)
	require.Equal(t, "active", got.Status, "a planned session is left alone")
}
