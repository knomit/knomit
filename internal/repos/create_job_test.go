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
