package repos

import (
	"context"
	"path/filepath"
	"sync"
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
