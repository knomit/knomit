package repos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// A repo with no origin never starts a reconcile loop, so its consensus
// branch used to sit on the root commit forever while every fact piled up on
// the agent branch. The local loop is what makes main mean the same thing
// here as on an origin-backed host — which is what a peer subscribing to this
// instance over /git reads.
func TestRunLocalReconcileLoop_AdvancesMainOnTick(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	ctx := context.Background()

	mainBefore, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/x.md",
		adoptFact("written on the agent branch"), "x", "")
	require.NoError(t, err)
	agentTip, err := svc.Branches().HeadCommit(ctx, ri.AgentBranch())
	require.NoError(t, err)
	require.NotEqual(t, mainBefore, agentTip, "the write must actually have moved the agent branch")

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go runLocalReconcileLoop(loopCtx, &wg, svc, ri.Name(), ri.AgentBranch(), 20*time.Millisecond)

	require.Eventually(t, func() bool {
		u, err := svc.Branches().HeadCommit(ctx, "main")
		return err == nil && u == agentTip
	}, 2*time.Second, 10*time.Millisecond, "main must reach the agent tip")

	// A second write advances again — the loop keeps following, it does not
	// just converge once at start.
	_, err = svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/y.md",
		adoptFact("a second fact"), "y", "")
	require.NoError(t, err)
	agentTip2, err := svc.Branches().HeadCommit(ctx, ri.AgentBranch())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		u, err := svc.Branches().HeadCommit(ctx, "main")
		return err == nil && u == agentTip2
	}, 2*time.Second, 10*time.Millisecond)

	// The advance went through notifyCommit, so main's index answers too.
	got, err := svc.Facts().ReadFact(ctx, "main", "kb/y.md", nil)
	require.NoError(t, err)
	require.NotEmpty(t, got.Content)

	cancel()
	wg.Wait()
}

// The loop runs once at start, so a restarted instance converges without
// waiting a full interval.
func TestRunLocalReconcileLoop_ConvergesAtStart(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	ctx := context.Background()

	_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/x.md",
		adoptFact("written before the loop starts"), "x", "")
	require.NoError(t, err)
	agentTip, err := svc.Branches().HeadCommit(ctx, ri.AgentBranch())
	require.NoError(t, err)

	loopCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	// An interval far longer than the test: only the start-up tick can
	// satisfy this.
	go runLocalReconcileLoop(loopCtx, &wg, svc, ri.Name(), ri.AgentBranch(), time.Hour)

	require.Eventually(t, func() bool {
		u, err := svc.Branches().HeadCommit(ctx, "main")
		return err == nil && u == agentTip
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	wg.Wait()
}

// The two loops are mutually exclusive by the same fact: a repo WITH an origin
// gets main from the remote, and the local loop must not also push it forward
// from the agent branch behind reconcileMain's back.
func TestRunLocalReconcileLoop_ExitsWhenTheRepoHasAnOrigin(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	ctx := context.Background()

	svc.SetOrigin(&store.Origin{URL: "https://example.invalid/kb.git", Branch: "main"})
	_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/x.md",
		adoptFact("agent-only"), "x", "")
	require.NoError(t, err)
	mainBefore, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go runLocalReconcileLoop(ctx, &wg, svc, ri.Name(), ri.AgentBranch(), 10*time.Millisecond)
	wg.Wait() // returns on its own; no cancel needed

	mainAfter, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, mainBefore, mainAfter, "an origin-backed repo's main is reconcileMain's to move")
}

// The wiring: a real origin-less repo, built through Manager.Create with
// background sync ENABLED, gets the local loop started for it. The unit tests
// above call the loop directly and would keep passing if nobody ever did.
func TestStartSyncLoops_StartsTheLocalLoopForAnOriginlessRepo(t *testing.T) {
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg: config.Config{
			Home:         home,
			OntologyRoot: "kb",
			Git:          config.GitConfig{LocalReconcileInterval: 20 * time.Millisecond},
		},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
		// DisableBackgroundSync deliberately NOT set: this is the path under test.
	})
	t.Cleanup(func() { m.Close() })
	ri := bootRepo(t, m)
	svc := testService(t, ri)
	ctx := context.Background()

	_, err := svc.Facts().WriteFact(ctx, ri.AgentBranch(), "kb/x.md",
		adoptFact("published by the local reconcile"), "x", "")
	require.NoError(t, err)
	agentTip, err := svc.Branches().HeadCommit(ctx, ri.AgentBranch())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		u, err := svc.Branches().HeadCommit(ctx, "main")
		return err == nil && u == agentTip
	}, 5*time.Second, 20*time.Millisecond, "nobody started the local reconcile loop")
}

// A store can hold refs/remotes/origin/main with no local main — a home moved
// between machines, or one written before local init bootstrapped it. A live
// instance was found that way, and the served advertisement takes HEAD from
// the LOCAL ref, so until this is repaired a peer cloning that repo gets an
// advertisement with no HEAD. The repair runs at open, before the startup
// reconcile, so an unreachable origin does not leave the endpoint headless for
// the length of the outage.
func TestOpen_BootstrapsAMissingLocalUpstreamFromOrigin(t *testing.T) {
	dir := t.TempDir()
	url := seedBareRemote(t, filepath.Join(dir, "remote.git"))

	m := newRemoteModeManager(t, dir)
	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "kb", Mode: "clone", Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	ctx := context.Background()

	var originTip string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		tip, herr := svc.Branches().HeadCommit(ctx, "main")
		require.NoError(t, herr)
		originTip = tip
		// Lose the local upstream while origin/main stays behind.
		require.NoError(t, svc.Branches().DropBranch(ctx, "main"))
		_, herr = svc.Branches().HeadCommit(ctx, "main")
		require.Error(t, herr, "the fixture must actually have removed main")
	}))
	m.Close()

	// Take the origin away. Without this the startup reconcile would fetch and
	// reconcileMain would recreate main by itself, and the test would pass
	// whether or not the open-time repair exists. An unreachable origin is
	// also the case the repair is FOR: it is what leaves the endpoint headless
	// for the length of the outage.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "remote.git")))

	// Reopen the same home: the registry re-opens "kb" through the same
	// builder path a server restart uses.
	m2 := newRemoteModeManager(t, dir)
	ri2 := m2.Get("kb")
	require.NotNil(t, ri2)
	require.NoError(t, ri2.WithRead(func(svc *store.Service) {
		got, herr := svc.Branches().HeadCommit(ctx, "main")
		require.NoError(t, herr, "local main was not bootstrapped from origin/main")
		require.Equal(t, originTip, got)
	}))
}

// A transient failure of the origin read must cost ONE tick, not the loop.
//
// GetRemote returns (nil, nil) when there is no origin, so an error there is a
// real failure of the status query — SQLITE_BUSY under concurrent writes is
// the plausible one. Treating it as "has an origin" and exiting meant one
// transient error stopped main advancing for the life of the process, and a
// subscribing peer silently stopped seeing facts: a symptom indistinguishable
// from a quiet repo.
func TestRunLocalReconcile_TransientOriginReadErrorSkipsOneTickOnly(t *testing.T) {
	var reads, advances atomic.Int64
	hasOrigin := func() (bool, error) {
		// Fail the FIRST read, which is the start-up one, then answer
		// truthfully forever after.
		if reads.Add(1) == 1 {
			return false, errors.New("database is locked")
		}
		return false, nil
	}
	advance := func() error {
		advances.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLocalReconcile(ctx, "repo", "agent/a", 10*time.Millisecond, hasOrigin, advance)
	}()

	require.Eventually(t, func() bool { return advances.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"the loop must keep ticking after a failed origin read, not stand down")
	cancel()
	<-done

	// The failed read produced NO advance: an unreadable answer decides
	// nothing, so it must not advance main behind reconcileMain's back either.
	require.Less(t, advances.Load(), reads.Load(),
		"the tick whose origin read failed must not have advanced anything")
}

// A definite "this repo has an origin" still exits — the two loops stay
// mutually exclusive by the same fact.
func TestRunLocalReconcile_ExitsOnADefiniteOrigin(t *testing.T) {
	var advances atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLocalReconcile(context.Background(), "repo", "agent/a", 10*time.Millisecond,
			func() (bool, error) { return true, nil },
			func() error { advances.Add(1); return nil })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit on a definite origin")
	}
	require.Zero(t, advances.Load())
}

// An origin that appears mid-life still stops the loop.
func TestRunLocalReconcile_ExitsWhenAnOriginAppearsLater(t *testing.T) {
	var reads atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLocalReconcile(context.Background(), "repo", "agent/a", 10*time.Millisecond,
			func() (bool, error) { return reads.Add(1) > 2, nil },
			func() error { return nil })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit once an origin appeared")
	}
}

// A subscription is read-only and has no agent branch; there is nothing to
// advance and the loop must not try.
func TestRunLocalReconcileLoop_ExitsWithoutAnAgentBranch(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	svc := testService(t, ri)

	var wg sync.WaitGroup
	wg.Add(1)
	go runLocalReconcileLoop(context.Background(), &wg, svc, ri.Name(), "", 10*time.Millisecond)
	wg.Wait()
}
