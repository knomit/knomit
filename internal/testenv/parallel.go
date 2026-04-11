package testenv

import (
	"fmt"
	"sync"
)

// Parallel runs fn n times concurrently, each invocation receiving its
// goroutine index [0, n). Blocks until every goroutine completes. Panics
// in child goroutines are recovered and reported via t.Fatal on the
// Storyboard's parent test, so a crash in one goroutine fails the test
// with a clear stack rather than taking down the whole process.
//
// Typical use in Category F concurrency tests:
//
//	sb.Parallel(50, func(i int) {
//	    agent.Write(fmt.Sprintf("kb/item%d.md", i), testenv.Fact("item"),
//	        fmt.Sprintf("add item%d", i))
//	})
//
// Parallel does NOT synchronize the goroutines at any internal barrier —
// they start as soon as the `go` statement runs. Tests that need all
// goroutines to start simultaneously should use a Barrier (below).
func (sb *Storyboard) Parallel(n int, fn func(i int)) {
	t := sb.t
	t.Helper()
	if n <= 0 {
		return
	}
	var wg sync.WaitGroup
	wg.Add(n)
	// Collect panics from goroutines so the parent test fails with a
	// clear report rather than crashing.
	var panicsMu sync.Mutex
	var panics []string

	for i := range n {
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicsMu.Lock()
					panics = append(panics, fmt.Sprintf("goroutine %d: %v", idx, r))
					panicsMu.Unlock()
				}
			}()
			fn(idx)
		}(i)
	}
	wg.Wait()

	if len(panics) > 0 {
		for _, p := range panics {
			t.Errorf("Parallel panic: %s", p)
		}
		t.FailNow()
	}
}

// Barrier is a sync.WaitGroup-like primitive that releases every caller
// simultaneously after `parties` goroutines have called Wait. Used for
// "all N goroutines start the critical section at the same instant"
// scenarios where Parallel's staggered-start behavior isn't tight enough.
//
//	barrier := sb.NewBarrier(10)
//	sb.Parallel(10, func(i int) {
//	    // setup work here runs in parallel with stagger
//	    barrier.Wait()
//	    // every goroutine enters this line simultaneously
//	    agent.Write(...)
//	})
//
// Unlike stdlib sync.WaitGroup, a Barrier is a one-shot synchronization
// point: once parties goroutines have arrived, the channel is closed and
// any later Wait() call returns immediately. This is deliberate — tests
// use one Barrier per test moment and reuse is a code smell.
type Barrier struct {
	mu      sync.Mutex
	parties int
	count   int
	ch      chan struct{}
}

// NewBarrier creates a Barrier that releases after `parties` goroutines
// call Wait. Zero or negative parties creates an already-released barrier.
func (sb *Storyboard) NewBarrier(parties int) *Barrier {
	b := &Barrier{parties: parties, ch: make(chan struct{})}
	if parties <= 0 {
		close(b.ch)
	}
	return b
}

// Wait blocks until the Barrier has been reached by `parties` goroutines.
// Once released, subsequent Wait calls return immediately.
func (b *Barrier) Wait() {
	b.mu.Lock()
	b.count++
	if b.count >= b.parties {
		// Last arrival — close the channel if it isn't already.
		select {
		case <-b.ch:
			// Already closed, nothing to do.
		default:
			close(b.ch)
		}
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	<-b.ch
}
