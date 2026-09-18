package repos

import (
	"context"
	"sync"
	"sync/atomic"
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
	release chan struct{}
	opened  sync.Once
	passed  atomic.Bool
}

func newIndexHealGate() *indexHealGate {
	return &indexHealGate{release: make(chan struct{})}
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
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	g.passed.Store(true)
}

// open lets the held heal proceed. Idempotent: the index mirror emits on a
// ticker, so the test callback that calls this runs more than once.
func (g *indexHealGate) open() {
	g.opened.Do(func() { close(g.release) })
}

// passedThrough reports whether the heal has reached the gate AND moved on,
// by either exit. It is what lets a fixture assert that the gate is still on
// the heal's path: if the hold is ever deleted or made non-blocking, this
// turns the resulting flake into a deterministic failure.
func (g *indexHealGate) passedThrough() bool {
	return g != nil && g.passed.Load()
}
