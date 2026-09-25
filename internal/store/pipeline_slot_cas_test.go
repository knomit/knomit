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
