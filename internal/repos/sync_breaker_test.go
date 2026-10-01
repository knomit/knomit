package repos

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// F21 S1: the sync circuit breaker. Rounds are driven by ri.wakeSync() with an
// auto-releasing countdown, and the breakers read a fake clock
// (syncHooks.now), so "open for 600 s" is an assertion about fake instants.
// A round is counted when its deferred dispatcher kick runs — the LAST thing
// a round does, after it has published its breakers.

// ---- B1, B2: the pure schedule and state machine.

// B1 OpenForSaturates: for every base and every opens the open period is
// positive, at most 30 min, non-decreasing in opens, exactly 30 min from 40
// opens on, and base 3 s gives 6, 12, 24 s. Sabotage: the naive
// `base << opens` (negative for 3 s at opens ≥ 32, zero at 1000) → red; drop
// the cap → red.
func TestBreaker_OpenForSaturates(t *testing.T) {
	opens := []int{}
	for i := 0; i <= 64; i++ {
		opens = append(opens, i)
	}
	opens = append(opens, 1000)
	for _, base := range []time.Duration{time.Second, 3 * time.Second, 300 * time.Second} {
		prev := time.Duration(0)
		for _, n := range opens {
			d := breakerOpenFor(base, n)
			require.Greater(t, d, time.Duration(0), "base %v opens %d", base, n)
			require.LessOrEqual(t, d, breakerMaxOpen, "base %v opens %d", base, n)
			require.GreaterOrEqual(t, d, prev, "non-decreasing: base %v opens %d", base, n)
			if n >= 40 {
				require.Equal(t, breakerMaxOpen, d, "saturated: base %v opens %d", base, n)
			}
			prev = d
		}
	}
	require.Equal(t, 6*time.Second, breakerOpenFor(3*time.Second, 1))
	require.Equal(t, 12*time.Second, breakerOpenFor(3*time.Second, 2))
	require.Equal(t, 24*time.Second, breakerOpenFor(3*time.Second, 3))
	require.Equal(t, 10*time.Minute, breakerOpenFor(300*time.Second, 1))
	require.Equal(t, 20*time.Minute, breakerOpenFor(300*time.Second, 2))
	require.Equal(t, 30*time.Minute, breakerOpenFor(300*time.Second, 3))
	require.Equal(t, breakerMaxOpen, breakerOpenFor(0, 1), "a non-positive base reads as the cap")
}

// B2 StateMachine: 2 failures stay closed; the 3rd opens for 2×base; allow is
// false before openUntil and true at it; a failed probe → opens 2 for 4×base;
// a successful probe → closed with fails 0. Sabotage: trip after 1; no reset
// on success; allow true while open → red.
func TestBreaker_StateMachine(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := 3 * time.Second
	var b breaker
	require.True(t, b.allow(t0))
	require.Equal(t, breakerClosed, b.state(t0))

	b = b.record(false, t0, base)
	b = b.record(false, t0, base)
	require.Equal(t, 2, b.fails)
	require.Equal(t, 0, b.opens, "two failures stay closed")
	require.True(t, b.allow(t0))

	b = b.record(false, t0, base)
	require.Equal(t, 1, b.opens, "the 3rd consecutive failure opens it")
	require.Equal(t, t0.Add(2*base), b.openUntil)
	require.False(t, b.allow(t0.Add(2*base-time.Nanosecond)), "skipped while open")
	require.Equal(t, breakerOpen, b.state(t0.Add(time.Second)))
	require.True(t, b.allow(t0.Add(2*base)), "the probe is allowed at openUntil")
	require.Equal(t, breakerHalfOpen, b.state(t0.Add(2*base)))

	probe := t0.Add(2 * base)
	b = b.record(false, probe, base)
	require.Equal(t, 2, b.opens, "a failed probe re-opens it")
	require.Equal(t, 4, b.fails)
	require.Equal(t, probe.Add(4*base), b.openUntil)
	require.False(t, b.allow(probe))

	b = b.record(true, probe.Add(4*base), base)
	require.Equal(t, breaker{}, b, "a successful probe closes it and resets the count")
	require.True(t, b.allow(probe))
}

// ---- The loop.

type brkClock struct {
	mu sync.Mutex
	t  time.Time
}

var brkT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func (c *brkClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *brkClock) set(offset time.Duration) {
	c.mu.Lock()
	c.t = brkT0.Add(offset)
	c.mu.Unlock()
}

// brkLoop is runReconcileLoop with counters on every seam the breakers sit
// behind.
type brkLoop struct {
	clock    *brkClock
	window   *windowSeam
	rounds   atomic.Int64 // completed rounds (the deferred kick)
	fetches  atomic.Int64 // attempted fetch steps
	pushes   atomic.Int64 // attempted push steps
	refused  atomic.Int64 // refusePush calls that refused
	refuse   atomic.Bool
	authFail atomic.Bool
	mode     *syncMode // the `sync` mode the loop follows (nil: today)
	// waits holds every between-rounds wait (F21 S2's seam), so a timer round
	// never fires on its own: every round is one the test woke.
	waits *waitSeam
}

func startBreakerLoop(t *testing.T, ri *RepoInstance, originRoot string, pre func(*brkLoop)) *brkLoop {
	t.Helper()
	l := &brkLoop{clock: &brkClock{t: brkT0}, window: &windowSeam{auto: true}, waits: &waitSeam{}}
	if pre != nil {
		pre(l)
	}
	setSyncHooks(t, syncHooks{
		window: l.window.open,
		wait:   l.waits.open,
		now:    l.clock.now,
		attempt: func(_, step string) {
			switch step {
			case "fetch":
				l.fetches.Add(1)
			case "push":
				l.pushes.Add(1)
			}
		},
		refusePush: func(string) error {
			if l.refuse.Load() {
				l.refused.Add(1)
				return errors.New("refused: agent/* is protected")
			}
			return nil
		},
	})
	auth := func(*store.Remote) (transport.AuthMethod, error) {
		if l.authFail.Load() {
			return nil, errors.New("no credential")
		}
		return nil, nil
	}
	kick := func() {
		ri.triggerKick()
		l.rounds.Add(1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go runReconcileLoop(ctx, &wg, testService(t, ri), ri.hub, ri.Name(), trigAgent, auth, originRoot, false, nil, kick, ri.syncWake, ri.breakers, l.mode)
	t.Cleanup(func() { cancel(); wg.Wait() })
	l.waitRounds(t, 1)
	return l
}

func (l *brkLoop) waitRounds(t *testing.T, n int64) {
	t.Helper()
	require.Eventually(t, func() bool { return l.rounds.Load() >= n }, 30*time.Second, 5*time.Millisecond,
		"round #%d never completed", n)
	require.Equal(t, n, l.rounds.Load(), "exactly %d round(s)", n)
}

// round wakes the loop once (ri.wakeSync, the `do: push` / knomit.push()
// path) and waits for that round to complete.
func (l *brkLoop) round(t *testing.T, ri *RepoInstance) {
	t.Helper()
	n := l.rounds.Load()
	ri.wakeSync()
	l.waitRounds(t, n+1)
}

// newFailingOriginRepo: a repo whose origin is example.invalid, both in the
// record and as the git remote, so every fetch and push really fails.
func newFailingOriginRepo(t *testing.T) *RepoInstance {
	t.Helper()
	_, ri := newTriggerRepo(t)
	svc := testService(t, ri)
	svc.SetOrigin(&store.Origin{URL: "https://example.invalid/kb.git", Branch: "main"})
	require.NoError(t, svc.ConfigureRemote("https://example.invalid/kb.git", "main", trigAgent))
	return ri
}

type syncEvents struct {
	mu   sync.Mutex
	sync []SyncEvent
	push []PushEvent
}

func (s *syncEvents) counts() (syncN, pushN int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sync), len(s.push)
}

func (s *syncEvents) statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.sync {
		out = append(out, e.Status)
	}
	for _, e := range s.push {
		out = append(out, e.Status)
	}
	return out
}

func subscribeSyncEvents(t *testing.T, ri *RepoInstance) *syncEvents {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events, _ := ri.TaskHub().Subscribe(ctx)
	s := &syncEvents{}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-events:
				s.mu.Lock()
				switch ev := e.(type) {
				case SyncEvent:
					s.sync = append(s.sync, ev)
				case PushEvent:
					s.push = append(s.push, ev)
				}
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// B3 PushOnlyFailureKeepsPulling: the fetch works and every push is refused.
// After 3 rounds the push breaker is open and the fetch breaker closed; over
// the next rounds the fetch runs EVERY round and the push is not attempted
// until openUntil, where exactly one probe runs. Sabotage: one shared breaker
// for the round → the fetch stops with the push → red.
func TestSyncBreaker_PushOnlyFailureKeepsPulling(t *testing.T) {
	ri, _, root := newOriginTriggerRepo(t)
	l := startBreakerLoop(t, ri, root, func(l *brkLoop) { l.refuse.Store(true) })
	l.round(t, ri)
	l.round(t, ri)
	require.Equal(t, int64(3), l.refused.Load(), "three refused pushes")
	fetch, push := ri.SyncBreakers()
	require.Equal(t, breakerOpen, push.State)
	require.Equal(t, 3, push.ConsecutiveFailures)
	require.Equal(t, breakerClosed, fetch.State, "a refused push does not touch the fetch breaker")
	require.NotNil(t, push.OpenUntil)
	require.Equal(t, brkT0.Add(600*time.Second), *push.OpenUntil, "2 × the 300 s interval")

	for i := 0; i < 4; i++ {
		l.clock.set(time.Duration(i+1) * 100 * time.Second) // ≤ 400 s: still open
		l.round(t, ri)
	}
	require.Equal(t, int64(7), l.fetches.Load(), "the fetch ran in every round")
	require.Equal(t, int64(3), l.refused.Load(), "no push was attempted while its breaker was open")
	require.Equal(t, int64(3), l.pushes.Load())

	l.clock.set(600 * time.Second)
	l.round(t, ri)
	require.Equal(t, int64(4), l.refused.Load(), "exactly one probe at openUntil")
	require.Equal(t, int64(8), l.fetches.Load())
	_, push = ri.SyncBreakers()
	require.Equal(t, breakerOpen, push.State)
	require.Equal(t, brkT0.Add(600*time.Second+1200*time.Second), *push.OpenUntil, "a failed probe doubles the period")
}

// B4 WokenRoundRespectsOpen (rev 2's M2): with the push breaker open, a round
// started by a WAKE — ri.wakeSync(), the path `do: push` and knomit.push()
// take — opens the countdown and runs, the fetch is attempted, the push is
// NOT, and the dispatcher kick still fires. Sabotage: the breaker consulted
// only for timer rounds → the woken round pushes → red.
func TestSyncBreaker_WokenRoundRespectsOpen(t *testing.T) {
	ri, _, root := newOriginTriggerRepo(t)
	l := startBreakerLoop(t, ri, root, func(l *brkLoop) { l.refuse.Store(true) })
	l.round(t, ri)
	l.round(t, ri)
	_, push := ri.SyncBreakers()
	require.Equal(t, breakerOpen, push.State)
	opens, fetches, refused := l.window.opened(), l.fetches.Load(), l.refused.Load()

	ri.wakeSync()
	l.waitRounds(t, 4)
	require.Equal(t, opens+1, l.window.opened(), "the wake opened the push countdown")
	require.Equal(t, fetches+1, l.fetches.Load(), "the woken round ran its fetch")
	require.Equal(t, refused, l.refused.Load(), "a woken round cannot bypass the open push breaker")
	require.Equal(t, int64(3), l.pushes.Load())
}

// B5 IntervalModeToo: no `sync` key, today's 300 s interval, a fetch that
// really fails (example.invalid): after 3 rounds the fetch step is skipped
// for exactly 600 s on the fake clock — skipped at 599 s, attempted at 600 s.
// Sabotage: no breaker at the default interval → the fetch at 599 s runs → red.
func TestSyncBreaker_IntervalModeToo(t *testing.T) {
	ri := newFailingOriginRepo(t)
	l := startBreakerLoop(t, ri, "", nil)
	l.round(t, ri)
	l.round(t, ri)
	require.Equal(t, int64(3), l.fetches.Load())
	fetch, _ := ri.SyncBreakers()
	require.Equal(t, breakerOpen, fetch.State)
	require.Equal(t, brkT0.Add(600*time.Second), *fetch.OpenUntil)

	l.clock.set(599 * time.Second)
	l.round(t, ri)
	require.Equal(t, int64(3), l.fetches.Load(), "skipped until openUntil")
	l.clock.set(600 * time.Second)
	l.round(t, ri)
	require.Equal(t, int64(4), l.fetches.Load(), "the probe runs at openUntil")
	fetch, _ = ri.SyncBreakers()
	require.Equal(t, breakerOpen, fetch.State, "the probe failed")
	require.Equal(t, 4, fetch.ConsecutiveFailures)
	require.Equal(t, brkT0.Add(600*time.Second+1200*time.Second), *fetch.OpenUntil)
}

// B6 HalfOpenRecovery: auth fails 3 rounds → the fetch breaker opens and the
// persisted status is the error; the credential is healed and openUntil
// passes → exactly one fetch attempt, which closes the breaker; the
// recovery-edge sync_ok broadcast fires exactly once (not again on the next
// idle round); the origin view reads closed. Sabotage: no probe (allow false
// while opens > 0) → stays open → red; no recovery broadcast (wasFailing
// ignored) → red.
func TestSyncBreaker_HalfOpenRecovery(t *testing.T) {
	ri, _, root := newOriginTriggerRepo(t)
	l := startBreakerLoop(t, ri, root, func(l *brkLoop) { l.authFail.Store(true) })
	l.round(t, ri)
	l.round(t, ri)
	fetch, push := ri.SyncBreakers()
	require.Equal(t, breakerOpen, fetch.State)
	require.Equal(t, breakerClosed, push.State, "an auth failure with the fetch due is not a push failure")
	require.Equal(t, int64(0), l.fetches.Load(), "auth failed before any fetch")

	l.authFail.Store(false)
	ev := subscribeSyncEvents(t, ri)
	l.clock.set(600 * time.Second)
	l.round(t, ri)
	require.Equal(t, int64(1), l.fetches.Load(), "exactly one probe")
	fetch, _ = ri.SyncBreakers()
	require.Equal(t, breakerClosed, fetch.State)
	require.Equal(t, 0, fetch.ConsecutiveFailures)
	require.Nil(t, fetch.OpenUntil)
	require.Eventually(t, func() bool { n, _ := ev.counts(); return n >= 1 }, 5*time.Second, 5*time.Millisecond,
		"the recovery edge broadcast sync_ok")

	l.round(t, ri)
	require.Equal(t, int64(2), l.fetches.Load(), "closed: back to every round")
	time.Sleep(pushQuiet)
	n, _ := ev.counts()
	require.Equal(t, 1, n, "the recovery edge is broadcast once")
	require.Contains(t, ev.statuses(), "sync_ok")
}

// B7 SkippedStepWritesNothing: with both breakers open, rounds write no
// status row and broadcast nothing, so the last REAL failure stays on the
// record; the origin view shows both open with open_until. Sabotage: a
// skipped step records a status → the sentinel is overwritten → red.
func TestSyncBreaker_SkippedStepWritesNothing(t *testing.T) {
	ri := newFailingOriginRepo(t)
	l := startBreakerLoop(t, ri, "", nil)
	l.round(t, ri)
	l.round(t, ri)
	fetch, push := ri.SyncBreakers()
	require.Equal(t, breakerOpen, fetch.State)
	require.Equal(t, breakerOpen, push.State)
	require.Equal(t, brkT0.Add(600*time.Second), *fetch.OpenUntil)
	require.Equal(t, brkT0.Add(600*time.Second), *push.OpenUntil)

	svc := testService(t, ri)
	require.NoError(t, svc.Remote().RecordSyncError("origin", "SENTINEL"))
	before, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, before.LastPushError)
	ev := subscribeSyncEvents(t, ri)

	for i := 1; i <= 3; i++ {
		l.clock.set(time.Duration(i) * 100 * time.Second)
		l.round(t, ri)
	}
	require.Equal(t, int64(3), l.fetches.Load())
	require.Equal(t, int64(3), l.pushes.Load())
	after, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Equal(t, "SENTINEL", *after.LastError, "a skipped fetch wrote a status row")
	require.Equal(t, *before.LastSyncAt, *after.LastSyncAt)
	require.Equal(t, *before.LastPushAt, *after.LastPushAt, "a skipped push wrote a status row")
	require.Equal(t, *before.LastPushError, *after.LastPushError)
	time.Sleep(pushQuiet)
	s, p := ev.counts()
	require.Equal(t, 0, s, "a skipped fetch broadcast")
	require.Equal(t, 0, p, "a skipped push broadcast")
}

// B8 AuthFailureWhileFetchOpenIsNotAPushFailure (review F1): after a healthy
// round, auth fails until the fetch breaker opens; further rounds with the
// fetch breaker open and the push due fail auth again. Those failures are
// charged to NOTHING: no push_error is broadcast (a banner no push_ok would
// ever lower, since no push status row records it), the push breaker stays
// closed with no failures, and no push is attempted. After the heal both
// breakers are closed and the push banner was never raised. Sabotage: charge
// the push breaker, or broadcast push_error, on that auth failure → red.
func TestSyncBreaker_AuthFailureWhileFetchOpenIsNotAPushFailure(t *testing.T) {
	ri, _, root := newOriginTriggerRepo(t)
	l := startBreakerLoop(t, ri, root, nil)
	require.Equal(t, int64(1), l.pushes.Load(), "the healthy first round pushed")

	l.authFail.Store(true)
	l.round(t, ri)
	l.round(t, ri)
	l.round(t, ri)
	fetch, _ := ri.SyncBreakers()
	require.Equal(t, breakerOpen, fetch.State)

	ev := subscribeSyncEvents(t, ri)
	for i := 1; i <= 3; i++ {
		l.clock.set(time.Duration(i) * 100 * time.Second) // the fetch breaker stays open
		l.round(t, ri)
	}
	_, push := ri.SyncBreakers()
	require.Equal(t, breakerClosed, push.State, "an auth failure is not a push failure")
	require.Equal(t, 0, push.ConsecutiveFailures)
	require.Equal(t, int64(1), l.pushes.Load(), "no push attempted without auth")

	l.authFail.Store(false)
	l.clock.set(600 * time.Second)
	l.round(t, ri)
	fetch, push = ri.SyncBreakers()
	require.Equal(t, breakerClosed, fetch.State)
	require.Equal(t, breakerClosed, push.State)
	time.Sleep(pushQuiet)
	ev.mu.Lock()
	pushEvents := append([]PushEvent(nil), ev.push...)
	ev.mu.Unlock()
	for _, e := range pushEvents {
		require.NotEqual(t, "push_error", e.Status, "a push banner was raised by an auth failure: %+v", pushEvents)
	}
	rem, err := testService(t, ri).Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rem.LastPushStatus)
	require.Equal(t, "ok", *rem.LastPushStatus)
}
