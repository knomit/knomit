package repos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mirrorIndexing narrates the heal and returns the state it ended in — and it
// renders the heal's OWN done/total, never an invented fraction.
func TestMirrorIndexing_NarratesProgressAndEndsReady(t *testing.T) {
	ri := &RepoInstance{}
	ri.markIndexing()
	ri.setIndexProgress(3, 12)

	var got []Event
	done := make(chan string, 1)
	go func() { done <- mirrorIndexing(context.Background(), ri, func(e Event) { got = append(got, e) }) }()

	// Let the mirror emit at least once at 3/12, then finish the heal.
	require.Eventually(t, func() bool { return len(got) > 0 }, 5*time.Second, 5*time.Millisecond)
	ri.setIndexProgress(12, 12)
	ri.markIndexReady()

	select {
	case state := <-done:
		require.Equal(t, IndexStateReady, state)
	case <-time.After(5 * time.Second):
		t.Fatal("mirrorIndexing did not return after the heal reported ready")
	}

	require.NotEmpty(t, got)
	first := got[0]
	require.Equal(t, PhaseIndex, first.Phase)
	require.Equal(t, "index", first.Step)
	require.Equal(t, IndexStateIndexing, first.IndexState)
	require.Equal(t, "indexing 3/12", first.Message, "the message carries the heal's own counts")
	// 3/12 of the band 95..99 is 96 — a value derived from the heal, which is
	// the whole point: a constant here would pass an "is it between 95 and 99"
	// assertion just as well.
	require.Equal(t, 96, first.Pct)
	require.False(t, first.Indeterminate, "indexing HAS a percent; only transfer does not")
}

// A heal that ends in error is reported as an error state — and the CREATE
// still succeeds, because the repo is there. mirrorIndexing's job is to say
// which, never to fail.
func TestMirrorIndexing_ReportsErrorWithoutFailing(t *testing.T) {
	ri := &RepoInstance{}
	ri.markIndexing()
	done := make(chan string, 1)
	go func() { done <- mirrorIndexing(context.Background(), ri, func(Event) {}) }()
	time.Sleep(10 * time.Millisecond)
	ri.markIndexFailed()

	select {
	case state := <-done:
		require.Equal(t, IndexStateError, state)
	case <-time.After(5 * time.Second):
		t.Fatal("mirrorIndexing did not return after the heal failed")
	}
}

// A heal that is STILL RUNNING when the job's context ends stops being
// narrated and nothing else: the mirror returns promptly, reporting the state
// it last saw. The heal has its own context and keeps going.
func TestMirrorIndexing_ContextEndsTheNarrationNotTheCreate(t *testing.T) {
	ri := &RepoInstance{}
	ri.markIndexing()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan string, 1)
	go func() { done <- mirrorIndexing(ctx, ri, func(Event) {}) }()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case state := <-done:
		require.Equal(t, IndexStateIndexing, state, "the last state it actually saw")
	case <-time.After(5 * time.Second):
		t.Fatal("mirrorIndexing ignored its context")
	}
	state, _, _ := ri.IndexStatus()
	require.Equal(t, IndexStateIndexing, state, "the heal itself is untouched")
}

// A heal that has already finished is not narrated at all — the mirror returns
// immediately with no index events. This is the ordinary answer under
// DisableBackgroundSync, where openOne heals inline before Add returns.
func TestMirrorIndexing_AlreadyReadyEmitsNothing(t *testing.T) {
	ri := &RepoInstance{}
	ri.markIndexReady()
	var got []Event
	require.Equal(t, IndexStateReady,
		mirrorIndexing(context.Background(), ri, func(e Event) { got = append(got, e) }))
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
