package repos

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/testsupport/testsigner"
)

// heldEmbedder blocks EmbedDocuments once armed, until the CALLER's context
// ends. Nothing ever releases it otherwise, so an index that reaches it can
// only leave by being cancelled — which is what lets a test tell "the index
// was cancelled" from "the index finished".
type heldEmbedder struct {
	testEmbedder
	armed     atomic.Bool
	started   chan struct{}
	startOnce sync.Once
	cancelled atomic.Bool
}

func (e *heldEmbedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	if !e.armed.Load() {
		return e.testEmbedder.EmbedDocuments(ctx, titles, bodies)
	}
	e.startOnce.Do(func() { close(e.started) })
	<-ctx.Done()
	e.cancelled.Store(true)
	return nil, ctx.Err()
}

// TestCreate_SyncEnterDoesNotParkBehindTheIndexRebuild pins that no stage
// Enter waits on the branch lock the Index job holds
// (kb/invariants/repos/create-job/index-before-sync).
//
// The losing interleaving is forced, not hoped for. The Index job's Rebuild
// takes lockBranch(upstream) first and is then held INSIDE it, in its
// embeddings phase, by an embedder that only a cancel releases; Sync.Enter is
// held until that has happened. A Sync.Enter that takes lockBranch(upstream)
// on the driver — as ensureLocalUpstream did from #411 until this fix — now
// parks for good, and both halves below fail:
//
//   - the create never reaches step "index": Mount cannot reply Ready, so
//     watchIndex never starts and the job sits on "register";
//   - CancelIndex, which only the driver handles, never returns.
//
// TestCancelCreate_DuringIndexLandsWithoutWaitingForTheIndex caught this only
// when the scheduler happened to let the index job win the lock (about one
// run in seven under -race, never with GOMAXPROCS=1).
func TestCreate_SyncEnterDoesNotParkBehindTheIndexRebuild(t *testing.T) {
	url := servedKnomitOrigin(t, 20)

	emb := &heldEmbedder{started: make(chan struct{})}
	var mgr atomic.Pointer[Manager]
	var heldPhase atomic.Value // the index phase when Sync.Enter was let go
	hook := func(stage StageID, point string, ctx context.Context) {
		switch {
		case stage == StageIndex && point == "index-job":
			// Arm only for the index job: nothing before it may block.
			emb.armed.Store(true)
		case stage == StageSync && point == "enter":
			select {
			case <-emb.started:
			case <-ctx.Done():
				return
			}
			if p := mgr.Load().Get("sub").indexProgress.Load(); p != nil {
				heldPhase.Store(p.phase)
			}
		}
	}

	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
		KeyPath:  filepath.Join(home, "agent.key"),
		Embedder: emb,
		Machine:  Options{Hook: hook},
	})
	mgr.Store(m)
	require.NoError(t, m.Start())
	// Close unmounts, which cancels the held index first, so even a parked
	// driver is released and the test never hangs on its way out.
	t.Cleanup(func() { _ = m.Close() })

	job := m.StartCreate(CreateSpec{Name: "sub", Mode: "subscribe", Origin: &OriginSpec{URL: url}})

	select {
	case <-emb.started:
	case <-time.After(60 * time.Second):
		st := job.Status()
		t.Fatalf("the index job never reached the embedder (job: state=%s step=%s message=%q)", st.State, st.Step, st.Message)
	}

	// The create narrates the index while it runs. Under the bug the job
	// stays on "register" for as long as the index holds the lock — here,
	// forever, since only a cancel releases it.
	require.Eventually(t, func() bool {
		st := job.Status()
		return st.State == CreateRunning && st.Step == "index" && st.IndexState == IndexStateIndexing
	}, 30*time.Second, 5*time.Millisecond,
		"the create never reported an in-flight index while still running: the mount walk is parked behind the index's branch lock")

	// ANTI-VACUITY: Sync.Enter was let go while Rebuild was inside its
	// embeddings phase, i.e. holding lockBranch. Read after the Eventually,
	// which cannot pass before Sync.Enter has returned.
	require.Equal(t, "embeddings", heldPhase.Load(),
		"Sync.Enter must be released while Rebuild holds the branch lock, or the test proves nothing")

	// The driver answers while the index still holds the lock. The index can
	// only end by cancellation (the embedder is never released), so a reply
	// here is a reply BEFORE the index finished.
	done := make(chan error, 1)
	go func() {
		_, err := m.Send(context.Background(), m.Get("sub"), CancelIndex())
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("CancelIndex did not return: the driver is parked behind the index's branch lock")
	}
	require.True(t, emb.cancelled.Load(), "the index must have ended by the cancel, not by finishing")
}
