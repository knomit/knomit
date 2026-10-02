package repos

import (
	"context"
	"sync"
)

// lifetimeGuard owns the start/stop state of one of a repo's LIFETIME
// components — the trigger dispatcher, the consensus merger and the experiment
// sweep, each of which runs on its own context derived from the manager
// context for as long as the repo is open.
//
// Each was once started exactly once, from activate() inside the heal
// goroutine, and every teardown waited indexWg before stopping it, which is
// what ordered the start before the stop. SwapStore broke "exactly once from
// one place" (issue #400): a swap that cancels the heal before activate() ran
// starts the components itself, from the request goroutine, where a
// concurrent Archive can be stopping them at the same moment. The guard is
// what keeps that safe:
//
//   - begin is a no-op when the component has already started, so a second
//     start can neither overwrite the first context's cancel (leaking its
//     goroutine past stop) nor launch a second loop;
//   - begin is a no-op once end has run, so a start that loses the race with
//     a teardown launches nothing for the teardown to miss;
//   - the WaitGroup Add happens under the same mutex end takes, so it is
//     ordered either before the stop's Wait or not at all.
type lifetimeGuard struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	started bool
	stopped bool
}

// begin derives the component's context from parent and registers one
// goroutine on wg, or reports false when the component already started or
// has been stopped — in which case the caller must launch nothing.
func (g *lifetimeGuard) begin(parent context.Context, wg *sync.WaitGroup) (context.Context, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || g.stopped {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	g.cancel = cancel
	g.started = true
	wg.Add(1)
	return ctx, true
}

// end marks the component stopped (so a later begin is refused) and cancels
// its context if it was started. The caller waits its WaitGroup afterwards.
func (g *lifetimeGuard) end() {
	g.mu.Lock()
	g.stopped = true
	cancel := g.cancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// running reports whether the component was started and not yet stopped.
// Only tests read it.
func (g *lifetimeGuard) running() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started && !g.stopped
}
