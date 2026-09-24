package knomitapi

import (
	"context"
	"fmt"
	"time"
)

// lockPollInterval is how often a waiting kb retries the credentials lock.
// A refresh takes one round trip, so this is short against it; it is a
// latency choice, not a measured property of anything.
const lockPollInterval = 25 * time.Millisecond

// pollLock calls try until it takes the lock, fails, or ctx ends.
func pollLock(ctx context.Context, try func() (bool, error)) error {
	t := time.NewTicker(lockPollInterval)
	defer t.Stop()
	for {
		ok, err := try()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the credentials lock (another kb is refreshing): %w", ctx.Err())
		case <-t.C:
		}
	}
}
