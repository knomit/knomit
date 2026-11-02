package repos

import (
	"sync"
	"sync/atomic"
	"time"
)

// The sync circuit breaker (F21 S1). User, 2026-10-01: "push and pull
// mechanisms - including the current ones, not realtime - should pass through
// a circuit breaker, I do not want to continuously lock the repo for
// consistent, repeated failures. The circuit breaker can have an exponential
// backoff with a predefined max time." Decision 7 (approved): normal pace for
// failures 1–2, open at the 3rd for 2× the normal wait, doubling per failed
// probe, capped at 30 minutes; separate fetch and push breakers.
//
// Each origin loop (runReconcileLoop) owns two breakers, one for the FETCH
// step (auth resolution plus Sync) and one for the PUSH step, and EVERY round
// consults them, whatever started it: the first round, the timer, or a wake
// (`do: push`, knomit.push(), the consensus merger). The state lives on the
// loop goroutine; rounds never overlap, so exactly one probe runs per open
// period. A loop restart (the Sync stage re-entering) or a server restart
// starts closed.
//
// What does NOT consult or change them: the one-shot network calls — the
// create-time push and the fleet operations' one push (join, leave, the
// master's publish). None of them loops.
// The local (no-origin) loop has no breaker: it does no network I/O.
//
// Nothing resets an open breaker early, with ONE scoped exception: a
// CREDENTIAL change (a stored secret's value replaced, a repo linked to or
// unlinked from a secret, or its pasted credential swapped) calls
// RepoInstance.credentialChanged, which bumps the reset generation and wakes
// the loop. The loop's next round sees the new generation, closes both
// breakers and zeroes its failure counts before consulting them, so the new
// credential is tried at once. A plain wake, a timer round, a URL or branch
// change and every other event leave the breakers as they are.

const (
	// breakerTripAfter is the number of consecutive failures that opens a
	// closed breaker.
	breakerTripAfter = 3
	// breakerMaxOpen is the "predefined max time": the longest a breaker
	// stays open. A code constant, not a config key.
	breakerMaxOpen = 30 * time.Minute
)

// Breaker states as the origin view reports them.
const (
	breakerClosed   = "closed"
	breakerOpen     = "open"
	breakerHalfOpen = "half_open"
)

// breaker is one step's state. The zero value is closed.
type breaker struct {
	fails     int       // consecutive failures; only a success resets it
	opens     int       // consecutive opens (0 = closed)
	openUntil time.Time // zero when closed
}

// allow reports whether the step may run now: closed, or open with openUntil
// reached (half-open: the one probe).
func (b breaker) allow(now time.Time) bool {
	return b.opens == 0 || !now.Before(b.openUntil)
}

// state names b's state at now.
func (b breaker) state(now time.Time) string {
	switch {
	case b.opens == 0:
		return breakerClosed
	case now.Before(b.openUntil):
		return breakerOpen
	default:
		return breakerHalfOpen
	}
}

// record returns the state after one ATTEMPT of the step. base is the round's
// normal wait (the loop's interval). A success closes the breaker. A failure
// while closed counts, and the breakerTripAfter-th opens it for 2×base; a
// failed half-open probe re-opens it for the next, doubled period.
func (b breaker) record(ok bool, now time.Time, base time.Duration) breaker {
	if ok {
		return breaker{}
	}
	b.fails++
	if b.opens == 0 && b.fails < breakerTripAfter {
		return b
	}
	b.opens++
	b.openUntil = now.Add(breakerOpenFor(base, b.opens))
	return b
}

// breakerOpenFor is how long a breaker stays open after its opens-th
// consecutive open: base doubled opens times, saturating at breakerMaxOpen.
// It doubles only while below the cap and never shifts, so it cannot
// overflow however large opens grows (a revoked token left for days); a
// non-positive base reads as the cap.
func breakerOpenFor(base time.Duration, opens int) time.Duration {
	d := base
	for i := 0; i < opens && d > 0 && d < breakerMaxOpen; i++ {
		d *= 2
	}
	if d > breakerMaxOpen || d <= 0 {
		d = breakerMaxOpen
	}
	return d
}

// BreakerView is one breaker as the origin view shows it.
type BreakerView struct {
	State               string     `json:"state"` // closed | open | half_open
	ConsecutiveFailures int        `json:"consecutive_failures"`
	OpenUntil           *time.Time `json:"open_until"`
}

func viewOf(b breaker, now time.Time) BreakerView {
	v := BreakerView{State: b.state(now), ConsecutiveFailures: b.fails}
	if b.opens > 0 {
		u := b.openUntil.UTC()
		v.OpenUntil = &u
	}
	return v
}

// syncBreakers is the read-only copy of the loop's two breakers that the
// origin view reads. The loop owns the state and publishes after each round;
// nothing else writes it. It lives on the RepoInstance (set once at
// construction), so it outlives a Sync stage restart, which publishes closed
// again.
type syncBreakers struct {
	mu          sync.Mutex
	fetch, push breaker
	// reset is the credential-change generation: requestReset bumps it and
	// the loop resets its breakers when it sees a value it has not seen.
	reset atomic.Uint64
}

// requestReset asks the loop to close both breakers on its next round (a
// credential change, see the file comment). Nil-safe.
func (s *syncBreakers) requestReset() {
	if s == nil {
		return
	}
	s.reset.Add(1)
}

// resetGen is the current reset generation; 0 for nil.
func (s *syncBreakers) resetGen() uint64 {
	if s == nil {
		return 0
	}
	return s.reset.Load()
}

func (s *syncBreakers) publish(fetch, push breaker) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fetch, s.push = fetch, push
	s.mu.Unlock()
}

func (s *syncBreakers) views(now time.Time) (fetch, push BreakerView) {
	if s == nil {
		return viewOf(breaker{}, now), viewOf(breaker{}, now)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return viewOf(s.fetch, now), viewOf(s.push, now)
}

// SyncBreakers reports the origin loop's fetch and push breakers. Both read
// closed before the loop's first round and on a repo with no origin.
func (ri *RepoInstance) SyncBreakers() (fetch, push BreakerView) {
	var s *syncBreakers
	if ri != nil {
		s = ri.breakers
	}
	return s.views(syncNow())
}
