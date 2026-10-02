package repos

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// newLifetimeManager is a manager whose repos build all three lifetime
// components: background sync left ON (DisableBackgroundSync suppresses the
// sweep and activates inline, so no heal would ever be in flight to cancel)
// and expiry_days set (the zero Config means never expire, so no sweep).
func newLifetimeManager(t *testing.T) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home:        t.TempDir(),
			Experiments: config.ExperimentsConfig{ExpiryDays: 30},
		},
		AgentBranch: "machine/test",
		Embedder:    testEmbedder{},
	})
	require.NoError(t, m.Start())
	// Bounded: the regression these tests catch is a loop teardown cannot
	// stop, which hangs Close. A test that already failed on that must not
	// then hang the whole package in its cleanup.
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { _ = m.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("Manager.Close did not return in cleanup: a lifetime loop leaked past its stop")
		}
	})
	return m
}

// TestSwapStore_DuringInitialHeal_StartsTheLifetimeComponents pins issue #400.
//
// SwapStore cancels the repo's initial index heal so it drains before the old
// database closes. The heal's cancelled exit skips activate() — correct for
// teardown, where the repo is going away, but a swap leaves the repo open and
// observed, and activate() is the only thing that starts the trigger
// dispatcher, the consensus merger and the experiment sweep. Before the fix a
// swap that landed during the heal left all three unstarted for the life of
// the process: no triggers fired, no consensus merged, no experiment expired.
//
// The heal is held at the test gate so the swap provably lands while it is in
// flight; the gate is opened only after the swap, so the heal leaves by the
// context arm — the cancelled exit under test.
func TestSwapStore_DuringInitialHeal_StartsTheLifetimeComponents(t *testing.T) {
	setExperimentSweepInterval(t, 20*time.Millisecond)
	m := newLifetimeManager(t)

	gate := newIndexHealGate()
	m.setIndexHealGate(gate)

	// Return from Create while the heal is still held: cancelling the
	// create's context from inside the index emit makes mirrorIndexing give
	// up waiting, as in TestManagerClose_DrainsAHealHeldAtTheTestGate.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sawIndexing atomic.Bool
	ri, err := m.Create(ctx, CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"},
		func(e Event) {
			if e.Phase != PhaseIndex {
				return
			}
			sawIndexing.Store(true)
			cancel()
		})
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.True(t, sawIndexing.Load(), "the create reported no index phase; the heal was never held")
	require.True(t, gate.waitArrived(10*time.Second), "the heal never reached the gate")
	require.False(t, gate.passedThrough(), "the heal must still be held when the swap starts")

	// Anti-vacuity: all three are built for this repo, so "not running" below
	// can only mean "never started".
	require.NotNil(t, ri.triggers, "fixture must build the trigger dispatcher")
	require.NotNil(t, ri.consensus, "fixture must build the consensus merger")
	require.NotNil(t, ri.sweep, "fixture must build the experiment sweep")
	require.False(t, ri.activated.Load(), "activate() must not have run while the heal is held")

	// The swap source: a copy of this repo's own database.
	require.NoError(t, ri.WithRead(func(svc *store.Service) { require.NoError(t, svc.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, m.RepoPath(ri.UID()), tmp)

	swapped := make(chan error, 1)
	go func() { swapped <- m.SwapStore(ri, tmp) }()
	select {
	case err := <-swapped:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("SwapStore did not return with a heal held at the gate")
	}
	gate.open()

	// The swap cancelled the heal (it left by the context arm, before the
	// gate was opened), so activate() never ran: this is the state the fix
	// has to recover from, not a race the test happened to win.
	require.True(t, gate.passedThrough(), "the swap must have drained the held heal")
	require.False(t, gate.leftViaRelease(), "the heal must have left by the context arm, i.e. been cancelled")
	require.False(t, ri.activated.Load(), "a cancelled heal must not have activated")
	// SwapStore does not mark the index state; the caller that rebuilds on
	// the new store owns the marks (the origin-session handler does).
	state, _, _ := ri.IndexStatus()
	require.Equal(t, IndexStateIndexing, state)

	// The dispatcher and the merger each kick themselves once at start, so a
	// started one completes a run without any write.
	require.Eventually(t, func() bool {
		_, seq := ri.triggers.runSequence()
		return seq > 0
	}, 10*time.Second, 20*time.Millisecond,
		"the trigger dispatcher never ran after a swap that cancelled the initial heal")
	require.Eventually(t, func() bool {
		return ri.consensus.snapshot().Runs > 0
	}, 10*time.Second, 20*time.Millisecond,
		"the consensus merger never ran after a swap that cancelled the initial heal")

	// The sweep: an experiment past expiry on the swapped-in store is expired.
	openExperimentOn(t, ri, "ancient", ri.AgentBranch())
	backdateExperiment(t, ri, "ancient", time.Now().Add(-40*24*time.Hour))
	require.Eventually(t, func() bool {
		return !experimentExists(t, ri, "ancient")
	}, 10*time.Second, 20*time.Millisecond,
		"the experiment sweep never ran after a swap that cancelled the initial heal")
}

// TestLifetimeComponents_StartTwiceIsANoOp: SwapStore may call start on a
// component activate() already started, or call it again on a later swap. A
// second start must not launch a second loop or replace the first loop's
// cancel — either would leave a goroutine teardown cannot stop (the stop
// cancels only the context it can see, then waits for both). Before the guard
// the sweep's second loop also closed `done` twice and panicked.
func TestLifetimeComponents_StartTwiceIsANoOp(t *testing.T) {
	m := newLifetimeManager(t)
	ri := createRepo(t, m, "work")
	require.Eventually(t, func() bool {
		s, _, _ := ri.IndexStatus()
		return s == IndexStateReady
	}, 30*time.Second, 20*time.Millisecond)
	require.True(t, ri.activated.Load(), "activate() must have run once the heal is ready")
	require.NotNil(t, ri.triggers)
	require.NotNil(t, ri.consensus)
	require.NotNil(t, ri.sweep)

	// A second start, on a context nothing will ever cancel: a second loop on
	// it could only be stopped by the stop that a replaced cancel defeats.
	ri.triggers.start(context.Background())
	ri.consensus.start(context.Background())
	ri.sweep.start(context.Background())

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Manager.Close did not return: a second start leaked a loop its stop cannot cancel")
	}

	// A start AFTER stop is refused too: a SwapStore racing a teardown must
	// not launch a loop the teardown has already stopped waiting for. The
	// dispatcher's self-kick would complete a run if it started.
	_, before := ri.triggers.runSequence()
	ri.triggers.start(context.Background())
	time.Sleep(200 * time.Millisecond)
	_, after := ri.triggers.runSequence()
	require.Equal(t, before, after, "a start after stop must launch nothing")
	require.False(t, ri.triggers.life.running())
	require.False(t, ri.consensus.life.running())
	require.False(t, ri.sweep.life.running())
}
