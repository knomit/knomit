package main

import (
	"sync"
	"time"
)

// observer debounces rapid notifications into a single callback invocation.
// Used to collapse rapid git writes into one idx.Sync + SSE push.
type observer struct {
	mu      sync.Mutex
	delay   time.Duration
	fn      func(hash string)
	timer   *time.Timer
	pending string
	stopped bool
}

func newObserver(delay time.Duration, fn func(hash string)) *observer {
	return &observer{delay: delay, fn: fn}
}

// Notify records a new commit hash and resets the debounce timer.
func (o *observer) Notify(hash string) {
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
		o.fn(hash)
	})
}

// Stop cancels any pending timer and flushes if a notification was pending.
func (o *observer) Stop() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.stopped = true
	if o.timer != nil {
		o.timer.Stop()
	}
	if o.pending != "" {
		hash := o.pending
		o.pending = ""
		o.mu.Unlock()
		o.fn(hash)
		o.mu.Lock()
	}
}
