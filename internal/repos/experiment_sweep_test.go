package repos

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestExperimentSweep_ErrorDoesNotStopTheLoop is the invariant the local
// reconcile loop earned the hard way: a transient failure costs one tick, not
// the process. A sweeper that returned here would stop expiring silently —
// experiments would just accumulate, which looks exactly like a busy user.
func TestExperimentSweep_ErrorDoesNotStopTheLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu    sync.Mutex
		calls int
	)
	done := make(chan struct{})
	expire := func(time.Time) ([]string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n >= 3 {
			select {
			case <-done:
			default:
				close(done)
			}
			return nil, nil
		}
		return nil, errors.New("database is locked")
	}

	go runExperimentSweep(ctx, "repo", 30, time.Millisecond, expire)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop stopped ticking after an error instead of retrying")
	}

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, calls, 3, "two failing ticks must be followed by a third")
}

// TestExperimentSweep_ReportsWhatItDroppedDespiteAnError: ExpireExperiments
// returns both — the names it managed to roll back and the joined per
// experiment failures — and the loop must not treat the error as the whole
// answer and discard the rest.
func TestExperimentSweep_ReportsWhatItDroppedDespiteAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(chan []string, 1)
	expire := func(time.Time) ([]string, error) {
		select {
		case seen <- []string{"gone"}:
		default:
		}
		return []string{"gone"}, errors.New("one of them would not drop")
	}

	go runExperimentSweep(ctx, "repo", 30, time.Hour, expire)

	select {
	case got := <-seen:
		require.Equal(t, []string{"gone"}, got)
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper never ran its first tick")
	}
}

// TestExperimentSweep_CutoffIsTheConfiguredWindow: the cutoff handed to the
// store is now minus expiry_days, computed fresh per tick. A window off by a
// factor (hours for days, say) would expire live work.
func TestExperimentSweep_CutoffIsTheConfiguredWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan time.Time, 1)
	expire := func(cutoff time.Time) ([]string, error) {
		select {
		case got <- cutoff:
		default:
		}
		return nil, nil
	}

	go runExperimentSweep(ctx, "repo", 7, time.Hour, expire)

	select {
	case cutoff := <-got:
		require.WithinDuration(t, time.Now().Add(-7*24*time.Hour), cutoff, time.Minute,
			"the cutoff is now minus expiry_days, in DAYS")
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper never ran its first tick")
	}
}

// TestExperimentSweep_DisabledByZeroExpiry: 0 means never expire and the loop
// must not run at all. A cutoff of `now` would roll back every experiment in
// the repo on the first tick, and there is no archive to recover them from.
func TestExperimentSweep_DisabledByZeroExpiry(t *testing.T) {
	called := make(chan struct{}, 1)
	expire := func(time.Time) ([]string, error) {
		called <- struct{}{}
		return nil, nil
	}

	// Returns immediately rather than spawning: run it inline, so a version
	// that ticked would deadlock the test rather than pass it.
	runExperimentSweep(context.Background(), "repo", 0, time.Millisecond, expire)

	select {
	case <-called:
		t.Fatal("expiry_days = 0 must not sweep at all")
	default:
	}
}

// TestExperimentSweep_EndToEndDropsOldKeepsYoung drives the real store through
// one tick: the aged experiment is gone, ref and all, and the fresh one is
// untouched. This is what proves the loop's cutoff and the store's comparison
// agree about which direction "older" runs.
func TestExperimentSweep_EndToEndDropsOldKeepsYoung(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()

	openExperimentOn(t, ri, "ancient", agent)
	openExperimentOn(t, ri, "current", agent)
	backdateExperiment(t, ri, "ancient", time.Now().Add(-40*24*time.Hour))
	backdateExperiment(t, ri, "current", time.Now().Add(-2*24*time.Hour))

	// Acquire, not WithRead: the goroutine outlives the call, and WithRead
	// releases the store generation as soon as its callback returns.
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	// The loop always ticks once at start, so one tick is all this needs.
	go runExperimentSweepLoop(ctx, &wg, svc, "work", 30, time.Hour)

	require.Eventually(t, func() bool {
		return !experimentExists(t, ri, "ancient")
	}, 5*time.Second, 10*time.Millisecond, "the aged experiment must be rolled back")

	require.True(t, experimentExists(t, ri, "current"),
		"and the fresh one must survive the same sweep")
	require.True(t, branchExists(t, ri, "exp/current"))
	require.False(t, branchExists(t, ri, "exp/ancient"), "expiry is a rollback: the ref goes too")

	cancel()
	wg.Wait()
}

// TestExperimentSweep_CommitRefreshesActivity: the other direction of the same
// clock. An experiment that would have expired stops being expirable the
// moment something commits to it.
func TestExperimentSweep_CommitRefreshesActivity(t *testing.T) {
	m := newLifecycleManager(t)
	ri := createRepo(t, m, "work")
	agent := ri.AgentBranch()
	branch := openExperimentOn(t, ri, "revived", agent)
	backdateExperiment(t, ri, "revived", time.Now().Add(-40*24*time.Hour))

	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		ctx := context.Background()
		_, err := svc.Facts().WriteFact(ctx, branch, "kb/gotchas/revive.md",
			"---\ntype: observation\n---\n# revive\n\nstill working on it\n", "keep alive", "test")
		require.NoError(t, err)

		dropped, err := svc.Experiments().ExpireExperiments(ctx, time.Now().Add(-30*24*time.Hour))
		require.NoError(t, err)
		require.Empty(t, dropped, "a commit on the experiment refreshed last_activity_at")
	}))

	require.True(t, experimentExists(t, ri, "revived"))
}

func backdateExperiment(t *testing.T, ri *RepoInstance, name string, when time.Time) {
	t.Helper()
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.SetExperimentActivityForTest(context.Background(), name, when))
	}))
}

func experimentExists(t *testing.T, ri *RepoInstance, name string) bool {
	t.Helper()
	var found bool
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, ok, err := svc.Experiments().GetExperiment(context.Background(), name)
		require.NoError(t, err)
		found = ok
	}))
	return found
}

func branchExists(t *testing.T, ri *RepoInstance, branch string) bool {
	t.Helper()
	var found bool
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Branches().HeadCommit(context.Background(), branch)
		found = err == nil
	}))
	return found
}
