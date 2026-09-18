package repos

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// indexHealGate holds the background index heal at one known point — after
// ri.markIndexing(), before healIndexBranches — so a test can observe the
// 'indexing' state instead of racing it.
//
// IT IS nil IN PRODUCTION. Manager.healGate is never set outside tests, and
// (*indexHealGate)(nil).hold returns immediately, so the heal goroutine's
// production path is unchanged apart from one nil load.
//
// WHY THIS EXISTS AT ALL, since a test hook in production code deserves an
// argument. Two tests need the create to report an index phase, and whether it
// does is a RACE, not a property:
//
//	openOne marks indexing and starts the heal, then Create does RecordRepoID
//	and (for a remote) ActivateSync, and only then calls mirrorIndexing —
//	which reads IndexStatus ONCE and returns without emitting if the heal has
//	already finished (lifecycle.go). With a tiny fixture the heal is
//	microseconds, so on a loaded runner it wins and both tests fail their
//	anti-vacuity asserts. That is CI jobs 105650652457, 105598133397 and
//	105665517180.
//
// The two fixes that do NOT work, and why:
//
//   - Make mirrorIndexing always emit once. An "indexing" event for a heal
//     that has already finished is a lie told to the UI, which is the thing
//     the mirror exists to avoid.
//   - Give the fixtures more corpus so the heal takes longer. That MOVES the
//     race, it does not remove it; the test would then pass because of how
//     fast the machine is, which is a measured environment and not a property.
//
// Holding the heal is the only option that makes the moment the tests already
// describe — "provably in flight" — actually provable.
type indexHealGate struct {
	arrived    chan struct{}
	arriveOnce sync.Once
	release    chan struct{}
	opened     sync.Once
	passed     atomic.Bool
	viaRelease atomic.Bool
}

func newIndexHealGate() *indexHealGate {
	return &indexHealGate{
		arrived: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// setIndexHealGate arms the gate for every repo this Manager opens from now
// on. Per-Manager, so a test that arms one does not reach across into any
// other test's manager — which is what lets these tests keep running beside
// the rest of the package. Call it before the Create under test.
//
// There is no production caller, and there should never be one.
func (m *Manager) setIndexHealGate(g *indexHealGate) {
	m.healGate.Store(g)
}

// hold blocks the heal goroutine until the test opens the gate, or until ctx
// is cancelled. nil-safe: a nil gate is the production case and returns at
// once.
//
// THE ctx CASE IS NOT DECORATION. ctx here is the repo's indexCtx, which
// Manager.Close / shutdown / SwapStore cancel and then indexWg.Wait() on. A
// gate that waited only on release would deadlock every teardown that happened
// while it was held — the close would wait for a goroutine that was waiting
// for a test that had already returned.
func (g *indexHealGate) hold(ctx context.Context) {
	if g == nil {
		return
	}
	// Announced BEFORE the select, so a test can distinguish "the heal is
	// parked here" from "the heal goroutine has not been scheduled yet". Both
	// leave passedThrough false, and conflating them is what made an earlier
	// version of the preset fixture inherit the very scheduling race the gate
	// exists to remove. See waitArrived.
	g.arriveOnce.Do(func() { close(g.arrived) })
	select {
	case <-g.release:
		g.viaRelease.Store(true)
	case <-ctx.Done():
	}
	g.passed.Store(true)
}

// leftViaRelease reports whether the heal left the gate because the TEST
// released it, rather than by teardown or by not having been held at all.
//
// It is a STRONGER kind of evidence than the other two, not a stronger
// probability. passedThrough() samples state at one instant, and arrived and
// passed are two events with an instruction window between them, so a fixture
// reading it can miss a hold that does not block — measured at 5 detections in
// 30 before waitArrived existed, and 28 in 30 after it. This asks which arm the
// heal actually took, which is a fact about what happened rather than about
// when we looked: measured 119 detections in 120 on the preset fixture and 30
// in 30 on the subscribe one.
//
// NOT 120 IN 120, and the residue is worth knowing rather than rounding away.
// hold() closes `arrived` and THEN selects — two statements. If the heal is
// preempted between them, the test's wait returns, the test goes on to open the
// gate, and the resumed non-blocking select finds `release` already closed and
// takes that arm. So the fixture closes its own window by being the thing that
// opens the gate. Closing that last gap would mean observing that a goroutine
// is parked, which Go does not expose, or more machinery in a test hook that is
// already a cost; 119/120 plus 30/30 in the same package means a hold that
// stops blocking reds CI on essentially every run, which is what this is for.
func (g *indexHealGate) leftViaRelease() bool {
	return g != nil && g.viaRelease.Load()
}

// waitArrived reports whether the heal reached the gate within d.
//
// BOUNDED, not a bare channel receive. If the hold is ever deleted from
// openOne the heal never arrives, and an unbounded wait here would hang the
// package and produce a -timeout panic instead of a named failure — which is
// the failure mode every fixture in this file is built to avoid. It is also
// deadlock-free by construction: a heal parked at the gate needs nothing from
// the caller in order to have arrived.
func (g *indexHealGate) waitArrived(d time.Duration) bool {
	if g == nil {
		return false
	}
	select {
	case <-g.arrived:
		return true
	case <-time.After(d):
		return false
	}
}

// open lets the held heal proceed. Idempotent: the index mirror emits on a
// ticker, so the test callback that calls this runs more than once.
func (g *indexHealGate) open() {
	g.opened.Do(func() { close(g.release) })
}

// passedThrough reports whether the heal has reached the gate AND moved on,
// by either exit. It is what lets a fixture assert that the gate is still on
// the heal's path: if the hold is ever deleted, this turns the resulting flake
// into a deterministic failure.
//
// READ IT TOGETHER WITH waitArrived. On its own, false is ambiguous — parked
// at the gate, or not started yet — so a fixture asserting "the heal was still
// held" must establish arrival first or it is asserting a scheduling accident.
func (g *indexHealGate) passedThrough() bool {
	return g != nil && g.passed.Load()
}
