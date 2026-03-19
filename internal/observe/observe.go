// Package observe provides a debouncing observer that collapses rapid
// notifications into a single callback invocation after a quiet period.
package observe

import (
	"sync"
	"sync/atomic"
	"time"
)

// Observer debounces rapid notifications into a single callback invocation.
// Used to collapse rapid git writes into one idx.Sync + SSE push, and guards
// against re-entrant calls when the callback is still running.
type Observer struct {
	mu      sync.Mutex
	delay   time.Duration
	fn      func(hash string)
	timer   *time.Timer
	pending string
	stopped bool
	running atomic.Bool // true while fn is executing; prevents re-entrant calls
}

// New creates a new Observer that calls fn after delay has elapsed since the
// last Notify call. fn is never called concurrently with itself.
func New(delay time.Duration, fn func(hash string)) *Observer {
	return &Observer{delay: delay, fn: fn}
}

// Notify records a new commit hash and resets the debounce timer.
// If fn is still running from a previous notification, this notification
// is recorded but fn will not be invoked until the current run finishes.
func (o *Observer) Notify(hash string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.stopped {
		return
	}

	o.pending = hash

	if o.timer != nil {
		o.timer.Stop()
	}
	o.timer = time.AfterFunc(o.delay, func() {
		o.mu.Lock()
		hash := o.pending
		o.pending = ""
		o.mu.Unlock()

		// Skip if a previous invocation is still running.
		if hash != "" && o.running.CompareAndSwap(false, true) {
			defer o.running.Store(false)
			o.fn(hash)
		}
	})
}

// Stop cancels any pending timer and flushes synchronously if a notification
// was pending. Safe to call from any goroutine.
func (o *Observer) Stop() {
	o.mu.Lock()
	o.stopped = true
	if o.timer != nil {
		o.timer.Stop()
	}
	hash := o.pending
	o.pending = ""
	o.mu.Unlock()

	// Call fn without holding the lock.
	if hash != "" && o.running.CompareAndSwap(false, true) {
		defer o.running.Store(false)
		o.fn(hash)
	}
}
