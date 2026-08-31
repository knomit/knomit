package repos

import (
	"sync"
	"time"
)

// commitObserver debounces rapid commit notifications into a single callback
// invocation after a quiet period. It collapses rapid git writes into one
// idx.Sync + SSE push, and guards against re-entrant calls when the callback is
// still running.
//
// This was internal/observe, a package with exactly one consumer — this one.
// The debounce window is a detail of how repos reacts to commits, not a
// capability any other layer needs, so it lives here unexported.
//
// DEFERRAL, NEVER DISCARD. The callback is heavyweight (Acquire →
// IndexManager().SyncLocked → hub.broadcastStatus), so after a large merge it
// runs for many seconds. Every state transition therefore happens under mu:
// `running` used to be an atomic the timer CAS'd, and a failed CAS threw the
// notification away — `pending` had already been cleared and nothing re-armed,
// so the heads of commits landing during a long run were lost and every browser
// tab stayed pinned to a stale commit until it was reloaded (issue #178).
type commitObserver struct {
	mu      sync.Mutex
	delay   time.Duration
	fn      func(hash string)
	timer   *time.Timer
	pending string
	stopped bool
	running bool // true while fn is executing; prevents re-entrant calls
}

// newCommitObserver creates an observer that calls fn after delay has elapsed
// since the last Notify call. fn is never called concurrently with itself.
func newCommitObserver(delay time.Duration, fn func(hash string)) *commitObserver {
	return &commitObserver{delay: delay, fn: fn}
}

// Notify records a new commit hash and resets the debounce timer.
// If fn is still running from a previous notification, this notification
// is recorded but fn will not be invoked until the current run finishes.
func (o *commitObserver) Notify(hash string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.stopped {
		return
	}

	o.pending = hash
	o.arm()
}

// arm restarts the debounce timer. The caller holds mu.
func (o *commitObserver) arm() {
	if o.timer != nil {
		o.timer.Stop()
	}
	o.timer = time.AfterFunc(o.delay, o.fire)
}

// fire is the debounce timer's callback: it delivers the pending hash, unless
// a previous run is still in flight.
//
// The in-flight case is the one that matters. It leaves `pending` exactly where
// it is and returns; the run that is still going re-arms the timer when it
// finishes, so the notification is DEFERRED, not dropped. That is what
// guarantees the LAST head always reaches the callback.
func (o *commitObserver) fire() {
	o.mu.Lock()
	if o.stopped || o.pending == "" || o.running {
		o.mu.Unlock()
		return
	}
	hash := o.pending
	o.pending = ""
	o.running = true
	o.mu.Unlock()

	o.fn(hash)

	o.mu.Lock()
	o.running = false
	// Anything that arrived while fn ran is still pending — give it a fresh
	// debounce window rather than losing it.
	if !o.stopped && o.pending != "" {
		o.arm()
	}
	o.mu.Unlock()
}

// Stop cancels any pending timer and flushes synchronously if a notification
// was pending. Safe to call from any goroutine.
//
// A flush is skipped when fn is already running: that run owns the callback,
// and flushing here would break "fn is never called concurrently with itself".
// The in-flight run's own re-arm is disabled by `stopped`, so its pending head
// is dropped — at shutdown there is no subscriber left to broadcast to, and the
// index sync that matters already ran inline in notifyCommit.
func (o *commitObserver) Stop() {
	o.mu.Lock()
	o.stopped = true
	if o.timer != nil {
		o.timer.Stop()
		o.timer = nil
	}
	hash := o.pending
	o.pending = ""
	flush := hash != "" && !o.running
	if flush {
		o.running = true
	}
	o.mu.Unlock()

	if !flush {
		return
	}

	// Call fn without holding the lock.
	o.fn(hash)

	o.mu.Lock()
	o.running = false
	o.mu.Unlock()
}
