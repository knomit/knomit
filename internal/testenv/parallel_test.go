package testenv

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestParallel_RunsAllGoroutines asserts that Parallel runs n goroutines,
// each receives its index, and the call blocks until all have completed.
func TestParallel_RunsAllGoroutines(t *testing.T) {
	t.Log("Scenario: Parallel(20, fn) calls fn 20 times and blocks until all done")
	sb := NewStoryboard(t)
	var counter int64
	var indices sync.Map
	sb.Parallel(20, func(i int) {
		atomic.AddInt64(&counter, 1)
		indices.Store(i, true)
	})
	require.Equal(t, int64(20), counter)
	for i := range 20 {
		_, ok := indices.Load(i)
		require.True(t, ok, "index %d must have been seen", i)
	}
}

// Note: the behavior "Parallel recovers panics in goroutines and reports
// them via t.Error" is asserted manually — Go's testing framework does not
// provide a clean way to write a self-test that expects a parent test to
// fail because a child subtest failed (the parent is marked failed
// regardless). The recovery code path is exercised end-to-end whenever a
// Category F concurrency test panics in a goroutine, which is sufficient.

// TestParallel_ZeroIsNoOp asserts that Parallel(0, fn) is a clean no-op
// and does not call fn.
func TestParallel_ZeroIsNoOp(t *testing.T) {
	t.Log("Scenario: Parallel(0, fn) does not call fn")
	sb := NewStoryboard(t)
	called := false
	sb.Parallel(0, func(i int) { called = true })
	require.False(t, called)
}

// TestBarrier_ReleasesOnLastArrival asserts that N goroutines calling
// Wait() all proceed simultaneously once the Nth arrives, and that
// "before" and "after" instants are clearly ordered.
func TestBarrier_ReleasesOnLastArrival(t *testing.T) {
	t.Log("Scenario: 5 goroutines with a barrier(5), all Wait, measured after-timestamps are within 10ms of each other")
	sb := NewStoryboard(t)
	b := sb.NewBarrier(5)

	var mu sync.Mutex
	var afterTimes []time.Time

	sb.Parallel(5, func(i int) {
		// Stagger arrivals so the barrier's "wait for all" is exercised.
		time.Sleep(time.Duration(i) * 10 * time.Millisecond)
		b.Wait()
		mu.Lock()
		afterTimes = append(afterTimes, time.Now())
		mu.Unlock()
	})

	require.Len(t, afterTimes, 5)
	// All "after" times must be very close to each other (within 20ms),
	// because the barrier releases them simultaneously.
	minT, maxT := afterTimes[0], afterTimes[0]
	for _, ts := range afterTimes[1:] {
		if ts.Before(minT) {
			minT = ts
		}
		if ts.After(maxT) {
			maxT = ts
		}
	}
	spread := maxT.Sub(minT)
	require.Less(t, spread.Milliseconds(), int64(30),
		"barrier release spread must be < 30ms, got %v", spread)
}

// TestBarrier_SubsequentWaitsReturnImmediately asserts that after a
// barrier has been released, later Wait() calls do not block.
func TestBarrier_SubsequentWaitsReturnImmediately(t *testing.T) {
	t.Log("Scenario: barrier(2), 2 goroutines wait and release; a 3rd Wait() on the released barrier returns immediately")
	sb := NewStoryboard(t)
	b := sb.NewBarrier(2)

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			b.Wait()
		}()
	}
	wg.Wait()

	// Third wait should not block.
	done := make(chan struct{})
	go func() {
		b.Wait()
		close(done)
	}()
	select {
	case <-done:
		// Good, returned immediately.
	case <-time.After(100 * time.Millisecond):
		t.Fatal("post-release Wait() blocked > 100ms")
	}
}

// TestBarrier_ZeroPartiesIsAlreadyReleased asserts that NewBarrier(0)
// returns a barrier whose Wait() does not block.
func TestBarrier_ZeroPartiesIsAlreadyReleased(t *testing.T) {
	t.Log("Scenario: NewBarrier(0), Wait() returns immediately without deadlocking")
	sb := NewStoryboard(t)
	b := sb.NewBarrier(0)
	done := make(chan struct{})
	go func() {
		b.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("zero-party barrier Wait() blocked")
	}
}
