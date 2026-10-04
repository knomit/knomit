package repos

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// heldIndexRepo creates a repo, then restarts its Index stage as a full
// rebuild held at the index job's hook, so the status reads "indexing" for as
// long as the test wants. It returns the repo and the gate.
func heldIndexRepo(t *testing.T) (*Manager, *RepoInstance, *gate) {
	t.Helper()
	g := newGate(StageIndex, "index-job")
	var armed atomic.Bool
	m := newTestManager(t)
	m.deps.Machine.Hook = func(s StageID, p string, ctx context.Context) {
		if armed.Load() {
			g.hook(s, p, ctx)
		}
	}
	ri := bootRepo(t, m)
	armed.Store(true)
	_, err := m.Send(context.Background(), ri, Rebuild(ri.AgentBranch()))
	require.NoError(t, err)
	g.waitArrived(t)
	t.Cleanup(g.open)
	require.Equal(t, IndexStateIndexing, ri.Status().Index.State)
	return m, ri, g
}

// reportProgress reports index progress exactly as the index job does: the
// counts into the instance, one coalesced internal event to the driver, which
// publishes.
func reportProgress(t *testing.T, ri *RepoInstance, done, total int) {
	t.Helper()
	ri.indexProgress.Store(&indexProgress{done: done, total: total})
	ri.machine.postProgress(ri.Status().gens[StageIndex])
	waitStatus(t, ri, "progress published", func(s Status) bool { return s.Index.Done == done && s.Index.Total == total })
}

// watchIndex narrates the index job and returns the state it ended in — and
// it renders the job's OWN done/total, never an invented fraction.
func TestWatchIndex_NarratesProgressAndEndsReady(t *testing.T) {
	_, ri, g := heldIndexRepo(t)
	reportProgress(t, ri, 3, 12)

	// The emit callback runs on the watcher's goroutine (in production, the
	// create's), so the collector is locked.
	var mu sync.Mutex
	var got []Event
	collect := func(e Event) { mu.Lock(); got = append(got, e); mu.Unlock() }
	seen := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }

	done := make(chan string, 1)
	go func() { done <- watchIndex(context.Background(), ri, collect) }()

	// Let it emit at least once at 3/12, then let the job finish.
	require.Eventually(t, func() bool { return seen() > 0 }, 5*time.Second, 5*time.Millisecond)
	g.open()

	select {
	case state := <-done:
		require.Equal(t, IndexStateReady, state)
	case <-time.After(30 * time.Second):
		t.Fatal("watchIndex did not return after the job reported ready")
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, got)
	first := got[0]
	require.Equal(t, PhaseIndex, first.Phase)
	require.Equal(t, "index", first.Step)
	require.Equal(t, IndexStateIndexing, first.IndexState)
	require.Equal(t, "indexing 3/12", first.Message, "the message carries the job's own counts")
	// 3/12 of the band 95..99 is 96 — a value derived from the job, which is
	// the whole point: a constant here would pass an "is it between 95 and 99"
	// assertion just as well.
	require.Equal(t, 96, first.Pct)
	require.False(t, first.Indeterminate, "indexing HAS a percent; only transfer does not")
}

// An index that ends in error is reported as an error state — and the CREATE
// still succeeds, because the repo is there. watchIndex's job is to say which,
// never to fail.
func TestWatchIndex_ReportsErrorWithoutFailing(t *testing.T) {
	m, ri, _ := heldIndexRepo(t)
	done := make(chan string, 1)
	go func() { done <- watchIndex(context.Background(), ri, func(Event) {}) }()
	_, err := m.Send(context.Background(), ri, CancelIndex())
	require.NoError(t, err)

	select {
	case state := <-done:
		require.Equal(t, IndexStateError, state)
	case <-time.After(30 * time.Second):
		t.Fatal("watchIndex did not return after the index ended in error")
	}
}

// An index job that is STILL RUNNING when the job's context ends stops being
// narrated and nothing else: watchIndex returns promptly, reporting the state
// it last saw. The index job runs under the machine and keeps going.
func TestWatchIndex_ContextEndsTheNarrationNotTheCreate(t *testing.T) {
	_, ri, _ := heldIndexRepo(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan string, 1)
	go func() { done <- watchIndex(ctx, ri, func(Event) {}) }()
	cancel()

	select {
	case state := <-done:
		require.Equal(t, IndexStateIndexing, state, "the last state it actually saw")
	case <-time.After(5 * time.Second):
		t.Fatal("watchIndex ignored its context")
	}
	require.Equal(t, IndexStateIndexing, ri.Status().Index.State, "the index job itself is untouched")
}

// An index that has already settled is not narrated at all — watchIndex
// returns immediately with no index events.
func TestWatchIndex_AlreadyReadyEmitsNothing(t *testing.T) {
	m := newTestManager(t)
	ri := bootRepo(t, m)
	var got []Event
	require.Equal(t, IndexStateReady,
		watchIndex(context.Background(), ri, func(e Event) { got = append(got, e) }))
	require.Empty(t, got)
}
func TestScaleIndexPct(t *testing.T) {
	// An uncounted heal reports the floor, never an invented fraction.
	require.Equal(t, indexPctFloor, scaleIndexPct(0, 0))
	require.Equal(t, indexPctFloor, scaleIndexPct(5, 0))
	require.Equal(t, indexPctFloor, scaleIndexPct(0, 10))
	require.Equal(t, 97, scaleIndexPct(5, 10))
	// It never reaches 100: 100 is what the TERMINAL event means.
	require.Equal(t, indexPctCeil, scaleIndexPct(10, 10))
	require.Equal(t, indexPctCeil, scaleIndexPct(11, 10))
}

// transferProgress forwards a remote's line as a MESSAGE on an indeterminate
// transfer event, keeping the live value of a carriage-return-overwritten
// progress line and dropping writes that carry no text.
func TestTransferProgress(t *testing.T) {
	var got []Event
	fwd := transferProgress(func(e Event) { got = append(got, e) }, "subscribe")

	fwd("knomit: sending 42 objects\n")
	fwd("Counting objects:  10%\rCounting objects:  90%\rCounting objects: 100%\r")
	fwd("   \n")
	fwd("")

	require.Len(t, got, 2, "blank writes are dropped, not emitted as empty messages")
	require.Equal(t, "knomit: sending 42 objects", got[0].Message)
	require.Equal(t, "Counting objects: 100%", got[1].Message, "the LAST update in the chunk is the live one")
	for _, e := range got {
		require.Equal(t, "subscribe", e.Step)
		require.Equal(t, PhaseTransfer, e.Phase)
		require.True(t, e.Indeterminate, "transfer has no honest percent")
		require.Empty(t, e.IndexState)
	}
}

// CreateJob.record keeps IndexState STICKY while overwriting everything else:
// the terminal "done" event carries the index state forward, and an ordinary
// event that says nothing about the index must not erase it.
func TestCreateJobRecord_IndexStateIsSticky(t *testing.T) {
	j := &CreateJob{done: make(chan struct{})}

	j.record(Event{Step: "subscribe", Phase: PhaseTransfer, Indeterminate: true, Message: "knomit: sending 9 objects"})
	st := j.Status()
	require.Equal(t, PhaseTransfer, st.Phase)
	require.True(t, st.Indeterminate)
	require.Empty(t, st.IndexState, "nothing has said anything about the index yet")

	j.record(Event{Step: "index", Phase: PhaseIndex, Message: "indexing 1/4", Pct: 96, IndexState: IndexStateIndexing})
	require.Equal(t, IndexStateIndexing, j.Status().IndexState)
	require.False(t, j.Status().Indeterminate, "indeterminate is overwritten, not sticky")

	j.record(Event{Step: "done", Phase: PhaseDone, Message: "repo ready", Pct: 100, IndexState: IndexStateReady})
	require.Equal(t, IndexStateReady, j.Status().IndexState)

	// An event with no index state leaves the last one standing.
	j.record(Event{Step: "whatever", Phase: PhaseDone, Message: "later"})
	require.Equal(t, IndexStateReady, j.Status().IndexState)
}
