package repos

import (
	"context"
	"path/filepath"
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
// lock the heal holds, so Create returns only after the index is ALREADY
// 'ready' (measured: 5/5 runs) — the cancel would then land on finished work
// and the test would pass under the very sabotage it exists to catch. Preset
// mode has no ActivateSync, so Create returns while the heal is still in
// flight (measured: 50/50 runs), which is the only arrangement in which the
// cancellation can do damage.
func TestStartCreate_JobDeadlineDoesNotPinTheIndexAtIndexing(t *testing.T) {
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "machine/test",
		Embedder:    testEmbedder{},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	ri, err := job.Result()
	require.NoError(t, err)
	require.NotNil(t, ri)

	// ANTI-VACUITY — asserted, not assumed. The create context is cancelled
	// the instant Create returns, so this test discriminates only while the
	// heal is still running at that instant. If a future change made the heal
	// finish first, this test would quietly stop catching the regression it
	// exists for; failing here says so out loud instead.
	state, _, _ := ri.IndexStatus()
	require.Equal(t, "indexing", state,
		"the index heal must still be in flight when the create context is cancelled, "+
			"or this test cannot detect the create deadline killing it")

	// The property: that cancellation is harmless. Pinned at 'indexing' is the
	// incident reproducing itself through the new deadline.
	require.Eventually(t, func() bool {
		s, _, _ := ri.IndexStatus()
		return s == "ready"
	}, 30*time.Second, 50*time.Millisecond,
		"the detached create's cancelled context must not stop the background index heal")
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
