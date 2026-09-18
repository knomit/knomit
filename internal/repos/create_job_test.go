package repos

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// TestStartCreate_DeadlineRollsBackToTerminalState pins requirement (2) of the
// #67 ruling: on TIMEOUT the repo must land in a legible TERMINAL state, not
// half-created limbo.
//
// The deadline is already expired when the worker goroutine starts, so Create
// unwinds at its first step boundary — deterministically, with no sleeping and
// no race. What the test then asserts is that the unwind is COMPLETE: the
// registry row Create inserts before that boundary is gone, no database file
// survives under repos/, and the name is free again. A create that failed but
// left any of those behind is precisely the limbo the ruling forbids.
func TestStartCreate_DeadlineRollsBackToTerminalState(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
		// Already expired by the time the goroutine runs.
		CreateTimeout: time.Nanosecond,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	ri, err := job.Result()
	require.Nil(t, ri)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a create that outruns its own deadline must fail WITH the deadline error")

	st := job.Status()
	require.Equal(t, CreateFailed, st.State)
	require.True(t, st.TimedOut, "the failure must be attributable to the deadline, not merely to 'an error'")
	require.False(t, st.FinishedAt.IsZero(), "a terminal job must carry when it became terminal")

	// The three faces of "terminal, not limbo" — all three, because each one
	// alone is survivable by a different half-rollback. A registry row with no
	// file reports as a MISSING repo offering to rehydrate itself; a file with
	// no row is an orphan; and a live map entry is a repo the API will serve.
	require.Nil(t, m.Get("work"), "the failed name must not be registered live")

	reg := m.Repos()
	require.NotNil(t, reg)
	_, found, gerr := reg.ByName("work")
	require.NoError(t, gerr)
	require.False(t, found, "the registry row inserted before the deadline check must be rolled back")

	dbs, gerr := filepath.Glob(filepath.Join(home, "repos", "*.db"))
	require.NoError(t, gerr)
	require.Empty(t, dbs, "the partial database must be removed, not left on disk")

	// And the name is genuinely free: the reservation was released, so the
	// same create can simply be retried. (A leaked reservation would answer
	// ErrCreateInFlight forever, which is limbo wearing a different hat.)
	retry := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	_, rerr := retry.Result()
	require.ErrorIs(t, rerr, context.DeadlineExceeded,
		"the retry must reach the deadline check, i.e. NOT be refused as already in flight")
}

// TestStartCreate_DoesNotUseTheCallersContext pins requirement (1): the create
// is detached. StartCreate takes NO context from its caller — there is nowhere
// to pass a request context in — and the work it starts runs on the manager's
// lifetime.
//
// The observable form: a create started while a caller-side context is already
// dead still produces a repo, and nobody has to observe the job for it to
// finish. Under the pre-fix shape (handler passing r.Context() into Create)
// the equivalent call produced nothing at all; the web-layer half of that is
// asserted in TestPostRepos_ClientDisconnectDoesNotAbortTheCreate.
func TestStartCreate_DoesNotUseTheCallersContext(t *testing.T) {
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, dead.Err()) // the caller's world is already over

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})

	// Nothing consumes progress here, on purpose: a detached create has no
	// consumer to wait for, so a create that only completes when someone is
	// watching would be the same "held by its observer" bug in a new place.
	select {
	case <-job.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("create never finished with nobody observing the job")
	}

	ri, err := job.Result()
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.NotNil(t, m.Get("work"))

	st := job.Status()
	require.Equal(t, CreateDone, st.State)
	require.Equal(t, 100, st.Pct, "a finished job must carry the last progress it recorded")
	require.Equal(t, "done", st.Step)
}

// TestStartCreate_JobDeadlineDoesNotPinTheIndexAtIndexing is the INCIDENT
// ORACLE for this fix.
//
// kb/incidents/repos/clone-create-index-stuck-indexing: the background index
// heal shared syncCtx/syncWg with the reconcile loop; ActivateSync's
// syncCancel()+syncWg.Wait() cancelled the in-flight heal, which returned on
// `ctx.Err() != nil` WITHOUT markIndexReady/markIndexFailed — pinning
// IndexStatus at 'indexing' forever. The repair gave the heal its own
// indexCtx/indexWg, cancelled only by real teardown.
//
// This fix introduces a NEW context that expires: the create's own deadline,
// which StartCreate also cancels (defer cancel()) the instant Create returns.
// If that context were ever threaded down into openOne — the obvious-looking
// "make the create properly cancellable" change — it would become the parent
// of indexCtx, and the deferred cancel would kill a still-running heal exactly
// as ActivateSync once did. Same pin, new cause.
//
// The structural reason it does not: Manager.Add → openOne takes no context at
// all and reads m.ctx. This test refuses to take that on trust.
//
// PRESET MODE IS LOAD-BEARING HERE, and clone mode is not interchangeable with
// it. In clone mode ActivateSync's synchronous reconcile blocks on the branch
// lock the heal holds, so Create would return only after the index is ALREADY
// 'ready' — the cancel would then land on finished work and the test would
// pass under the very sabotage it exists to catch. Preset mode has no
// ActivateSync, so the heal is still in flight when the mirror first reads it.
//
// HOW THIS TEST DISCRIMINATES, and why the arrangement changed. It used to
// call StartCreate and assert, right after Result(), that IndexStatus was
// still 'indexing' — relying on Create returning while the heal ran (measured
// 50/50 runs on preset). The index mirror ended that: Create now WAITS for the
// heal before reporting done, so that window is gone by construction and the
// old anti-vacuity assertion could never hold again.
//
// The property is unchanged. WHAT MADE THE OLD ARRANGEMENT RACY, and what the
// gate does about it — the comment here used to claim this was "DETERMINISTIC
// rather than by timing … no measurement, no flake", and that was false. It is
// what CI jobs 105650652457 and 105598133397 failed on.
//
// The mirror emits an index event only while the heal is in the 'indexing'
// state, which is true and was mistaken for a guarantee that it emits AT ALL.
// openOne marks indexing and starts the heal; Create then does RecordRepoID
// before it calls mirrorIndexing; and mirrorIndexing reads IndexStatus ONCE and
// returns without emitting if the heal has already finished. With a 0-fact
// preset the heal is microseconds, so on a loaded runner it finished first, no
// index event was emitted, and the anti-vacuity assert below fired. The test
// was not detecting a regression — it was losing a race.
//
// indexHealGate removes the race instead of making it less likely: the heal
// blocks between markIndexing() and healIndexBranches until this test opens
// the gate, so "the heal is in flight when the mirror looks" is now TRUE BY
// CONSTRUCTION rather than usually. The fixture is unchanged — a bigger corpus
// would only have moved the race, which is a measured environment and not a
// property. See index_heal_gate.go.
//
// So the cancel still lands at a moment when the heal is PROVABLY in flight,
// and the same question is asked: did the heal survive? If the create context
// were ever threaded into openOne, it would parent indexCtx, the cancel would
// land on a running heal, and the heal would return without
// markIndexReady/markIndexFailed — pinned at 'indexing', the incident
// reproducing itself through a new context.
func TestStartCreate_JobDeadlineDoesNotPinTheIndexAtIndexing(t *testing.T) {
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
		Embedder:    testEmbedder{},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Armed BEFORE the Create, so the heal this create starts is held at the
	// gate the moment it is launched.
	gate := newIndexHealGate()
	m.setIndexHealGate(gate)

	// Create is called directly rather than through StartCreate so the test
	// OWNS the create's context — which is what StartCreate's own deadline is,
	// and what its defer cancel() ends the instant Create returns.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawIndexing, healAlreadyPastGate atomic.Bool
	ri, err := m.Create(ctx, CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"},
		func(e Event) {
			if e.Phase != PhaseIndex {
				return
			}
			// The mirror only emits this while IndexStatus reads 'indexing',
			// and the gate is why that state is still there to be read.
			if !sawIndexing.Swap(true) {
				// ARRIVAL FIRST, then the reading. passedThrough() is false in
				// two situations a fixture must not conflate: the heal is
				// parked at the gate (what this test means) and the heal
				// goroutine has not been scheduled yet (what often happens).
				// Reading it without establishing arrival asserts a scheduling
				// accident — and it inherits the exact race the gate removes,
				// which is why the non-blocking-hold mutation used to escape
				// this test most of the time.
				require.True(t, gate.waitArrived(10*time.Second),
					"the heal never reached the gate, so the index event cannot "+
						"have come from the gate holding it")
				healAlreadyPastGate.Store(gate.passedThrough())
			}
			// Order matters. Cancel while the heal is still held, THEN release
			// it — that is the whole point of the arrangement, and opening
			// first would let the heal run ahead of the cancel.
			cancel()
			gate.open()
		})
	require.NoError(t, err)
	require.NotNil(t, ri)

	// ANTI-VACUITY — asserted, not assumed. Without an index event the cancel
	// never happened at a discriminating moment and this test proves nothing;
	// failing here says so out loud instead of passing quietly.
	require.True(t, sawIndexing.Load(),
		"the create reported no index phase, so the context was never cancelled "+
			"while the heal was running and this test cannot detect the regression")

	// THE GATE WAS STILL SHUT when the mirror emitted, so the index event came
	// from the heal being held rather than from winning a race. This is the
	// half that fails if the hold is ever made non-blocking.
	require.False(t, healAlreadyPastGate.Load(),
		"the heal was already past the gate when the mirror emitted, so the "+
			"index event was luck rather than the gate holding the heal in place")

	require.Error(t, ctx.Err(), "the create context must be cancelled by now")

	// The property: that cancellation is harmless. Pinned at 'indexing' is the
	// incident reproducing itself through the create's own context.
	require.Eventually(t, func() bool {
		s, _, _ := ri.IndexStatus()
		return s == "ready"
	}, 30*time.Second, 50*time.Millisecond,
		"the detached create's cancelled context must not stop the background index heal")

	// THE OTHER HALF, and it is asserted HERE rather than beside its twin
	// above for a reason worth writing down: opening the gate does not
	// schedule the heal goroutine. Create returns the instant the cancelled
	// context breaks mirrorIndexing's select, which can be — and on a first
	// run of this test was — before the released heal has run a single
	// instruction. Reading the flag there tested the Go scheduler, not the
	// gate. By the time the index reads 'ready' the heal has provably run to
	// completion, so this is the first point where the answer is stable.
	//
	// What it catches: delete the hold from openOne and the heal never touches
	// the gate, so this fails by name instead of the test quietly going back to
	// racing the heal the way it did in CI.
	require.True(t, gate.passedThrough(),
		"the heal completed without ever passing through the gate; if the hold "+
			"was removed from openOne this test is racing the heal again, "+
			"exactly as it was before")

	// AND IT LEFT BY THE RELEASE ARM — the deterministic half. The two
	// assertions above sample state at an instant, and arrival and passage are
	// two events with an instruction window between them, so a hold that does
	// not block can slip through that window (measured: 2 escapes in 30). The
	// ARM the heal took has no window: a non-blocking hold takes neither case,
	// so this is false every time it is broken. The heal cannot have left by
	// the ctx arm here — the create's cancel does not touch indexCtx, which is
	// the property this whole test exists to prove.
	require.True(t, gate.leftViaRelease(),
		"the heal did not leave the gate by the release arm, so it was never "+
			"actually held: the gate must BLOCK, not merely be on the path")
}

// TestManagerClose_DrainsInFlightCreate is the third instance of an invariant
// Close already enforces twice: drain in-flight background work BEFORE touching
// the handles it uses.
//
// TestManagerClose_WaitsForBackgroundIndex pinned it for the index heal (PR #82
// review finding #1 — "Manager.Close ran svc.Close() while the heal was still
// issuing SQL on the same *sql.DB — a use-after-close"), and
// TestClose_WaitsForInFlightAcquire pins it for an outstanding Acquire. #67
// introduced a THIRD background worker — the detached create — and never
// registered it with the drain. The create runs on m.ctx, which Close neither
// cancels nor waits for, so it keeps issuing SQL against a control.db that
// Close has already closed (observed: "registry: sql: database is closed") and
// keeps writing files into a directory the caller is about to remove. That is
// the CI flake this fixes, and the Manager.Close edge deferred from #67.
//
// Close CANCELS before waiting rather than waiting the create out: a create is
// bounded by CreateTimeout, and blocking shutdown for half an hour is the wrong
// shutdown semantics. Cancelling makes the wait short AND makes the rollback
// complete, because the create unwinds through its own boundary checks and
// cleanup() while control.db is still open.
// CLONE MODE IS THE FIXTURE so the create is slow enough to still be running
// when Close arrives; the anti-vacuity assertion below checks that directly.
//
// WHAT MADE THIS TEST DETERMINISTIC WAS THE ASSERTION, NOT THE FIXTURE, and the
// history is worth keeping because the obvious version does not work. The first
// form asked a TIMING question — "was the job done when Close returned?" — and
// measured, with the drain removed, 27/30 red on preset and 28/30 on a
// one-fact clone. Scaling the remote to 200 facts did NOT help (20/30): the
// margin was never the problem.
//
// The reason is that Close BREAKING the create is what makes the create finish
// quickly, so the symptom partially masks itself — the timing check missed
// precisely the runs where teardown won hardest. Asking about the create's
// OUTCOME instead is 30/30 red without the drain and 0/20 with it, on the
// CHEAPEST fixture. The 200-fact remote was built, measured, and thrown away.
func TestManagerClose_DrainsInFlightCreate(t *testing.T) {
	root := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch: "machine/test",
		Embedder:    testEmbedder{},
	})
	require.NoError(t, m.Start())
	url := seedBareRemoteWithFact(t, filepath.Join(root, "remote.git"))

	job := m.StartCreate(CreateSpec{Name: "cloned", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main"}})

	// ANTI-VACUITY. The test says something only if the create is STILL RUNNING
	// when Close is called — if it had already finished, Close would have
	// nothing to drain and would pass with no drain implemented at all.
	require.Equal(t, CreateRunning, job.Status().State,
		"the create must still be in flight when Close is called, or this test "+
			"cannot detect whether Close drains it")

	require.NoError(t, m.Close())

	// THE PRIMARY PROPERTY, and it is about the create's OUTCOME rather than
	// about timing: Close must not DESTROY an in-flight create. Measured, with
	// the drain removed, the create is killed 12 times out of 12 — in two
	// flavours, roughly evenly split:
	//
	//   "repo manager is not running"      (Close nilled the handles first)
	//   "registry: sql: database is closed" (Close closed control.db under it)
	//
	// With the drain, 10 out of 10 report `context canceled` — a create that
	// was asked to stop and stopped, which is the whole difference between an
	// orderly shutdown and a use-after-close.
	//
	// This assertion replaced a timing one ("was the job done when Close
	// returned?"), which looked reasonable and was only ~65% reliable. The
	// reason is worth keeping: Close BREAKING the create is what makes the
	// create finish quickly, so the symptom partially masks itself and the
	// timing check missed exactly the runs where teardown won the race hardest.
	_, cerr := job.Result()
	if cerr != nil {
		require.NotErrorIs(t, cerr, ErrManagerStopped,
			"Close nilled the control handles while a create was still running")
		require.NotContains(t, cerr.Error(), "database is closed",
			"Close closed control.db while a create was still issuing SQL on it — "+
				"the use-after-close this drain exists to prevent")
	}

	// SECONDARY, and strictly true whenever the drain is present: Close did not
	// return until the create was terminal. Checked without blocking, because a
	// receive that had to wait would be this test doing the draining Close is
	// supposed to have done. Weaker than the outcome check above (it catches
	// only about half the unfixed runs), kept because it can never fail while
	// the drain is working.
	select {
	case <-job.Done():
	default:
		t.Fatal("Close returned while a detached create was still in flight")
	}
	require.NotEqual(t, CreateRunning, job.Status().State,
		"a drained create must be terminal, not merely unobserved")
}

// TestManagerClose_DrainsAHealHeldAtTheTestGate pins the one constraint the
// gate adds to teardown, and it is a constraint on the GATE rather than on the
// manager: hold() must watch indexCtx, not only its release channel.
//
// The failure it rules out is a deadlock, which is the worst shape a test hook
// can take. Every teardown path — Manager.Close, Archive→shutdown, SwapStore —
// cancels indexCtx and then indexWg.Wait()s on the heal goroutine before
// closing the SQLite handle it is using (see the comment on the heal launch in
// manager.go, and kb/incidents/repos/clone-create-index-stuck-indexing for why
// the heal has its own context at all). A gate that waited only on release
// would hang every one of them whenever a test returned with the gate still
// shut — and it would hang them forever, with no assertion to name the cause.
//
// So: hold a heal at the gate, never open it, and require that Close still
// returns.
func TestManagerClose_DrainsAHealHeldAtTheTestGate(t *testing.T) {
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
		Embedder:    testEmbedder{},
	})
	require.NoError(t, m.Start())

	gate := newIndexHealGate()
	m.setIndexHealGate(gate)

	// The create must RETURN while the heal is still held, or there is nothing
	// to close over. Cancelling from inside the index emit is what does that:
	// mirrorIndexing's select takes the ctx.Done() arm and Create reports the
	// last state it saw. The gate is deliberately NOT opened.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var sawIndexing atomic.Bool
	_, err := m.Create(ctx, CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"},
		func(e Event) {
			if e.Phase != PhaseIndex {
				return
			}
			sawIndexing.Store(true)
			cancel()
		})
	require.NoError(t, err)
	require.True(t, sawIndexing.Load(),
		"the create reported no index phase, so the heal was never held at the "+
			"gate and this test would prove nothing about closing over one")

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()

	select {
	case cerr := <-closed:
		require.NoError(t, cerr)
	case <-time.After(30 * time.Second):
		// Not require.Eventually: a hang is the thing under test, and this
		// wants to say so in words rather than report a boolean that never
		// went true.
		t.Fatal("Manager.Close did not return with a heal held at the index gate — " +
			"indexHealGate.hold must select on indexCtx as well as its release " +
			"channel, or every teardown deadlocks behind a gate no one will open")
	}

	// The heal left the gate by the CONTEXT arm, not the release arm: nothing
	// ever opened this gate. That is the exit path the test exists for.
	require.True(t, gate.passedThrough(),
		"the heal never left the gate, so Close returned without draining it")
}
