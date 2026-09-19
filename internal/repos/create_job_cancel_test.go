package repos

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

// requireNoTrace asserts the three faces of "the repo is not there" plus the
// fourth that cancel adds: no live instance, no registry row, no database
// file, and NO ARCHIVE ROW. The last is what distinguishes cancel from
// archive — a cancelled create must not be findable, restorable or
// purgeable afterwards, because "archived" is a trace.
func requireNoTrace(t *testing.T, m *Manager, home, name string) {
	t.Helper()
	require.Nil(t, m.Get(name), "the cancelled name must not be registered live")

	reg := m.Repos()
	require.NotNil(t, reg)
	_, found, err := reg.ByName(name)
	require.NoError(t, err)
	require.False(t, found, "the registry row must be gone")

	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, archived, "cancel must leave no archive row behind")

	dbs, err := filepath.Glob(filepath.Join(home, "repos", "*.db"))
	require.NoError(t, err)
	require.Empty(t, dbs, "the database file must be removed, not left on disk")
}

// awaitCancelled polls a job to the CreateCancelled terminal state.
//
// POLLING, not <-job.Done(): a job that had already finished closed that
// channel when it finished, so waiting on it would return instantly and every
// assertion after it would race the delete goroutine. The state is the only
// thing that moves on this path.
func awaitCancelled(t *testing.T, job *CreateJob) CreateStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		st := job.Status()
		if st.State != CreateRunning && st.State != CreateCancelling {
			require.Equal(t, CreateCancelled, st.State,
				"the job left cancelling for something other than cancelled: %v", st.Err)
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never left %s", st.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCancelCreate_DoneJobDeletesTheRepoWithoutATrace is the case the user
// actually hits: the create landed, they realise it was the wrong mode, and
// cancel must undo it — not archive it, DELETE it. The job then reports
// cancelled, and the name is free to be created again.
func TestCancelCreate_DoneJobDeletesTheRepoWithoutATrace(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	ri, err := job.Result()
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, CreateDone, job.Status().State)
	require.NotNil(t, m.Get("work"))

	require.NoError(t, m.CancelCreate(job.ID()))

	// THE CALL RETURNED BEFORE THE DELETE HAPPENED. That is the contract, not
	// an accident of timing: CancelCreate records the request and comes back,
	// so an HTTP caller is never held for it. The job reports `cancelling`
	// until the goroutine it started is done.
	require.Equal(t, CreateCancelling, job.Status().State)

	// A second cancel WHILE CANCELLING is idempotent, not an error — a button
	// reading "Cancelling…" must not fail when pressed twice.
	require.NoError(t, m.CancelCreate(job.ID()))

	st := awaitCancelled(t, job)
	require.ErrorIs(t, st.Err, ErrCreateCancelled)
	requireNoTrace(t, m, home, "work")

	// Cancelling once it is CANCELLED has nothing left to undo.
	require.ErrorIs(t, m.CancelCreate(job.ID()), ErrCreateFinished)

	// And the name is genuinely free again — the whole reason to cancel a
	// wrong-mode create is to redo it.
	again := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	_, err = again.Result()
	require.NoError(t, err)
	require.NotNil(t, m.Get("work"))
}

// TestCancelCreate_RunningJobEndsCancelledAndLeavesNoTrace pins the running
// case. Which boundary the cancel lands on is timing — before Create's first
// ctx check, or after m.Add when only the delete can honour it — and the test
// deliberately does not care: BOTH must end in CreateCancelled with nothing
// left behind. A cancel that reported "done" because it arrived late is the
// bug this guards against.
func TestCancelCreate_RunningJobEndsCancelledAndLeavesNoTrace(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	require.NoError(t, m.CancelCreate(job.ID()))

	select {
	case <-job.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("cancelled create never reached a terminal state")
	}
	ri, err := job.Result()
	require.Nil(t, ri, "a cancelled job must not hand back a repo")
	require.ErrorIs(t, err, ErrCreateCancelled)
	require.NotErrorIs(t, err, context.Canceled,
		"a user cancel must be reported as ErrCreateCancelled, distinguishable from shutdown")

	st := job.Status()
	require.Equal(t, CreateCancelled, st.State)
	require.False(t, st.TimedOut)
	require.False(t, st.FinishedAt.IsZero())
	requireNoTrace(t, m, home, "work")
}

// TestCancelCreate_RefusesWhatItCannotUndo: a failed job already rolled back,
// an unknown id has nothing behind it, and a done job whose repo has since
// been replaced by another of the same name is not this job's to delete.
func TestCancelCreate_RefusesWhatItCannotUndo(t *testing.T) {
	t.Run("failed job", func(t *testing.T) {
		m := New(context.Background(), Deps{
			Cfg:           config.Config{Home: t.TempDir()},
			AgentBranch:   "machine/test",
			CreateTimeout: time.Nanosecond, // expired before the worker runs
		})
		require.NoError(t, m.Start())
		t.Cleanup(func() { _ = m.Close() })

		job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
		_, err := job.Result()
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, CreateFailed, job.Status().State)

		require.ErrorIs(t, m.CancelCreate(job.ID()), ErrCreateFinished)
		require.Equal(t, CreateFailed, job.Status().State, "a refused cancel must not relabel the failure")
	})

	t.Run("unknown id", func(t *testing.T) {
		m := New(context.Background(), Deps{
			Cfg:         config.Config{Home: t.TempDir()},
			AgentBranch: "machine/test",
		})
		require.NoError(t, m.Start())
		t.Cleanup(func() { _ = m.Close() })
		require.ErrorIs(t, m.CancelCreate("never-started"), ErrCreateUnknown)
	})

	t.Run("done job whose repo was replaced", func(t *testing.T) {
		m := New(context.Background(), Deps{
			Cfg:         config.Config{Home: t.TempDir()},
			AgentBranch: "machine/test",
		})
		require.NoError(t, m.Start())
		t.Cleanup(func() { _ = m.Close() })

		first := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
		_, err := first.Result()
		require.NoError(t, err)

		// The user archives it and creates a NEW repo under the same name.
		_, err = m.Archive("work")
		require.NoError(t, err)
		second := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
		_, err = second.Result()
		require.NoError(t, err)
		replacement := m.Get("work")
		require.NotNil(t, replacement)

		// Cancelling the FIRST job must not delete the second job's repo.
		require.ErrorIs(t, m.CancelCreate(first.ID()), ErrCreateFinished)
		require.Same(t, replacement, m.Get("work"), "the replacement repo must survive")
		require.Equal(t, CreateDone, first.Status().State)
	})
}

// countSubscriptions reads repo_subscriptions straight from the control
// database, rather than through Origins, because Origins.Get LEFT JOINs that
// table to derive Mode: a stale subscription row there is invisible to it once
// the origin row is gone, which is exactly the leak this counts.
func countSubscriptions(t *testing.T, m *Manager, uid string) int {
	t.Helper()
	var n int
	require.NoError(t, m.Repos().DB().QueryRow(
		`SELECT COUNT(*) FROM repo_subscriptions WHERE repo_uid = ?`, uid).Scan(&n))
	return n
}

// TestCancelCreate_LeavesNoOriginOrSubscriptionRow closes the last hole in
// "no trace": the rows in control.db that are keyed by the repo's UID rather
// than by its name.
//
// requireNoTrace cannot see these. It asks the registry, the archive and the
// filesystem — all keyed by NAME — so a repo_origins or repo_subscriptions row
// orphaned under a uid nothing points at any more would satisfy every one of
// its assertions while still sitting in the database, carrying a URL and an
// encrypted token, and still matching Origins.ActiveRepoWithURL. That last one
// is what makes an orphan more than untidy: a later create against the same
// remote asks "is some repo already subscribed to this URL?" and a stale row
// answers yes for a repo that no longer exists.
//
// Nothing in the delete path deletes them EXPLICITLY, and that is the point of
// testing it. They go by ON DELETE CASCADE from repos(uid) — declared in the
// control baseline and in the subscriptions migration — which only fires
// because the control database opens with _foreign_keys=on. That is three
// separate facts in three separate files, none of them local to Purge, and any
// one of them changing breaks this silently. SUBSCRIBE mode is used because it
// is the only mode that populates BOTH tables.
func TestCancelCreate_LeavesNoOriginOrSubscriptionRow(t *testing.T) {
	url := servedKnomitOrigin(t, 3)

	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
	})
	require.NoError(t, m.Start())
	// Registered AFTER the origin server's own cleanup so it runs BEFORE it
	// (LIFO): the manager's sync loops must stop talking to the origin before
	// the origin goes away.
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}})
	ri, err := job.Result()
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, CreateDone, job.Status().State)

	rec, found, err := m.Repos().ByName("sub")
	require.NoError(t, err)
	require.True(t, found)
	uid := rec.UID
	require.NotEmpty(t, uid)

	// PRECONDITION, not decoration: both rows must actually be there before
	// the cancel, or the assertions after it would pass against a repo that
	// never wrote them and this test would prove nothing.
	org, err := m.Origins().Get(uid)
	require.NoError(t, err)
	require.NotNil(t, org, "a subscribe create must persist an origin row to begin with")
	require.Equal(t, OriginModeSubscribe, org.Mode)
	require.Equal(t, url, org.URL)
	require.Equal(t, 1, countSubscriptions(t, m, uid),
		"a subscribe create must persist a subscription row to begin with")

	require.NoError(t, m.CancelCreate(job.ID()))
	require.Equal(t, CreateCancelling, job.Status().State,
		"CancelCreate must record and return, not delete inline")
	awaitCancelled(t, job)

	org, err = m.Origins().Get(uid)
	require.NoError(t, err)
	require.Nil(t, org, "the origin row outlived the repo it belongs to")
	require.Equal(t, 0, countSubscriptions(t, m, uid),
		"the subscription row outlived the repo it belongs to")

	// The URL is genuinely free again: a stale row here would make the next
	// create against this remote believe it is already subscribed.
	claimed, err := m.Origins().ActiveRepoWithURL(url)
	require.NoError(t, err)
	require.Empty(t, claimed, "a deleted repo still claims its origin URL")

	requireNoTrace(t, m, home, "sub")
}

// TestCancelCreate_RunningJobReportsCancellingBeforeCancelled pins the state a
// client draws between the click and the outcome.
//
// The gap is the whole reason this state exists. A running create stops at its
// next STEP BOUNDARY, and the step in flight may be a clone taking minutes;
// with no cancelling state the job kept reporting whatever progress line it
// last emitted, so the UI went on drawing "Reading the remote, 40%" after the
// user pressed Cancel and read as frozen.
func TestCancelCreate_RunningJobReportsCancellingBeforeCancelled(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	require.NoError(t, m.CancelCreate(job.ID()))

	// Recorded synchronously: the state has already moved by the time the
	// call returns, so a client that re-reads immediately sees `cancelling`
	// rather than the stale progress line.
	st := job.Status()
	require.Contains(t, []CreateState{CreateCancelling, CreateCancelled}, st.State,
		"a cancelled create must never still report `running`")

	<-job.Done()
	require.Equal(t, CreateCancelled, job.Status().State)
	requireNoTrace(t, m, home, "work")
}

// TestCreateJobs_OmitsCancelledButKeepsFailed is the list contract behind the
// user's "what's the point, I KNOW it was cancelled".
//
// A cancelled job has nothing left to tell a repository list: the repo is
// gone and the outcome is the one that was asked for, so a row saying
// "cancelled — dismiss" is the UI reporting the user's own decision back to
// them and then asking them to acknowledge it. A FAILED job is the opposite —
// it carries an error nobody has read yet, and its row is the only place that
// explanation exists.
//
// The omission is LIST-LEVEL. CreateJobByID still answers, which is what lets
// the wizard that asked for the cancel poll through to the terminal state
// instead of falling off a 404 the moment it succeeds.
func TestCreateJobs_OmitsCancelledButKeepsFailed(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home},
		AgentBranch: "machine/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	_, err := job.Result()
	require.NoError(t, err)

	// Listed while it is live...
	require.Len(t, m.CreateJobs(), 1)

	require.NoError(t, m.CancelCreate(job.ID()))
	// ...and STILL listed while cancelling: the work is in flight, and a row
	// that vanished on the click would lose the "cancelling" report that the
	// rail is supposed to show.
	found := false
	for _, st := range m.CreateJobs() {
		if st.ID == job.ID() {
			found = true
			require.Equal(t, CreateCancelling, st.State)
		}
	}
	require.True(t, found, "a cancelling job must stay in the list")

	awaitCancelled(t, job)

	require.Empty(t, m.CreateJobs(), "a cancelled job must not be listed")
	// But it is still READABLE by id, which is what the wizard polls.
	byID, ok := m.CreateJobByID(job.ID())
	require.True(t, ok, "a cancelled job must remain readable by id until its TTL")
	require.Equal(t, CreateCancelled, byID.Status().State)

	// A FAILED job stays in the list: its error is the only place the reason
	// for it lives.
	fm := New(context.Background(), Deps{
		Cfg:           config.Config{Home: t.TempDir()},
		AgentBranch:   "machine/test",
		CreateTimeout: time.Nanosecond,
	})
	require.NoError(t, fm.Start())
	t.Cleanup(func() { _ = fm.Close() })
	failed := fm.StartCreate(CreateSpec{Name: "work", Mode: "preset", OntologyPreset: "default"})
	_, err = failed.Result()
	require.Error(t, err)
	require.Len(t, fm.CreateJobs(), 1, "a failed job must stay listed — it carries an error worth reading")
}

// TestCreate_CancelledDuringIndexSkipsSyncActivation is the UNIT half of the
// late-cancel fix, and it names the exact call that made a cancel take minutes.
//
// ActivateSync runs one SYNCHRONOUS reconcile, and that reconcile takes
// lockBranch(upstream) — the lock the background heal holds for the whole of
// the index. So a cancel arriving DURING the index used to do nothing visible:
// mirrorIndexing returned promptly on ctx.Done(), Create then parked in
// ActivateSync behind the heal's lock, and the worker goroutine could not run
// DeleteRepo until the entire index had finished. Observed live on a 697-fact
// repo: state=cancelling, step=sync, pct=99, for minutes.
//
// The assertion is on the STEP, not on elapsed time: a create whose context
// died before sync activation must never emit "sync" at all. That is a
// property, not a race, so it cannot flake — whereas timing the call would.
// The cancel is fired from inside an index-phase emit, the same trick
// TestStartCreate_JobDeadlineDoesNotPinTheIndexAtIndexing uses, because that
// is the one moment the heal is PROVABLY in flight and therefore holding the
// lock this test is about.
func TestCreate_CancelledDuringIndexSkipsSyncActivation(t *testing.T) {
	url := servedKnomitOrigin(t, 3)

	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var steps []string
	var sawIndex bool
	ri, err := m.Create(ctx, CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}},
		func(e Event) {
			mu.Lock()
			steps = append(steps, e.Step)
			mu.Unlock()
			if e.Phase == PhaseIndex {
				// The heal is in flight right now, holding the branch lock.
				mu.Lock()
				sawIndex = true
				mu.Unlock()
				cancel()
			}
		})
	require.NoError(t, err)
	// The repo IS registered, and Create must hand it back — the worker
	// goroutine can only delete what it is given.
	require.NotNil(t, ri, "a cancelled create past m.Add must still return its repo to be deleted")

	mu.Lock()
	defer mu.Unlock()
	// ANTI-VACUITY: without an index event the cancel never landed at a
	// discriminating moment and this test proves nothing.
	require.True(t, sawIndex,
		"no index phase was reported, so the context was never cancelled while the "+
			"heal held the branch lock; this test cannot detect the regression")
	require.NotContains(t, steps, "sync",
		"a create cancelled before sync activation must not activate sync on a repo "+
			"that is about to be deleted: %v", steps)
}

// TestCancelCreate_DuringIndexLandsWithoutWaitingForTheIndex is the end-to-end
// half: the user's actual complaint, which was that a late cancel appeared to
// do nothing at all.
//
// It asserts the cancel lands FAST RELATIVE TO THE INDEX rather than against a
// wall-clock constant. The comparison is what makes it meaningful and what
// keeps it honest on a slow machine: the same manager then indexes a second
// repo from the same fixture, and the cancel must complete in well under that.
// A fixed "under 5 seconds" would be a machine-speed assertion, and on the
// broken code a fast enough box could satisfy it.
func TestCancelCreate_DuringIndexLandsWithoutWaitingForTheIndex(t *testing.T) {
	// TWO fixtures, not one. A repo already subscribed to an origin URL makes
	// a second subscribe to the same URL refuse, so a baseline create against
	// the same remote would starve the measured one of an index entirely.
	refURL := servedKnomitOrigin(t, 200)
	url := servedKnomitOrigin(t, 200)

	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// A create we let run to completion, to measure what a full index costs on
	// THIS machine with THIS fixture.
	baseline := time.Now()
	ref := m.StartCreate(CreateSpec{Name: "ref", Mode: "subscribe", Origin: &OriginSpec{URL: refURL}})
	_, err := ref.Result()
	require.NoError(t, err)
	fullCreate := time.Since(baseline)
	t.Logf("uncancelled create with a full index: %s", fullCreate)

	// Now the same create again, cancelled once it reaches the index phase.
	job := m.StartCreate(CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}})
	// THE LATE WINDOW, taken from the JOB rather than from the registry.
	//
	// CreateStatus.IndexState is STICKY — record() keeps the last non-empty
	// value — so once the mirror has reported 'indexing' this stays true and a
	// poll cannot step over it. The job's own status is also the thing a client
	// watches, which makes it the honest trigger: this is the state the user
	// was looking at when they pressed Cancel.
	require.Eventually(t, func() bool {
		st := job.Status()
		return st.IndexState == IndexStateIndexing && st.State == CreateRunning
	}, 120*time.Second, 5*time.Millisecond,
		"the create never reported an in-flight index while still running")

	start := time.Now()
	require.NoError(t, m.CancelCreate(job.ID()))

	// Poll to terminal, WATCHING THE STEP.
	//
	// The step is what discriminates, not the clock. Under the bug the job
	// parks on step "sync" — Create sits in ActivateSync behind the heal's
	// branch lock — and stays there for the whole remaining index; that is
	// precisely what was observed live (state=cancelling, step=sync, pct=99,
	// for minutes). So a cancelled job that ever reports "sync" has waited on
	// the lock, whatever the wall clock happened to say on this machine.
	//
	// The elapsed time is logged and bounded too, but loosely: a timing
	// assertion alone is a machine-speed assertion, and on a fixture whose
	// index is nearly finished by the time the cancel lands it passes under
	// the broken code as readily as under the fixed code.
	var sawSync bool
	for {
		st := job.Status()
		if st.Step == "sync" {
			sawSync = true
		}
		if st.State != CreateRunning && st.State != CreateCancelling {
			require.Equal(t, CreateCancelled, st.State)
			break
		}
		require.Less(t, time.Since(start), 120*time.Second, "the cancel never landed")
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(start)
	t.Logf("cancel during the index landed in %s (a full create takes %s)", elapsed, fullCreate)

	require.False(t, sawSync,
		"the cancelled job activated sync, so it waited on the branch lock the "+
			"index heal holds — the whole reason a late cancel appeared to do nothing")
	require.Less(t, elapsed, fullCreate,
		"a cancel during the index took as long as a full create (%s)", elapsed)

	require.Nil(t, m.Get("sub"))
	archived, aerr := m.ListArchived()
	require.NoError(t, aerr)
	require.Empty(t, archived, "cancel must leave no archive row behind")
	_, found, rerr := m.Repos().ByName("sub")
	require.NoError(t, rerr)
	require.False(t, found, "the registry row must be gone")
}
