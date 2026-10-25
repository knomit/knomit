package repos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos/events"
)

// THE TERMINAL EVENT MUST SURVIVE THE THROTTLE. Progress is capped at one per
// repo per second; a terminal event inside that window must still arrive,
// because dropping it is not a dropped frame — it is the UI stuck in exactly
// the state this mechanism exists to clear.
//
// Progress is reported through the index job's own path (the counts, then one
// coalesced internal event the driver publishes), inside one throttle window;
// the terminal is CancelIndex's result, published from the same point.
func TestRepoIndexEvents_TerminalSurvivesTheProgressThrottle(t *testing.T) {
	m, ri, _ := heldIndexRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := m.RepoEventHub().Subscribe(ctx)

	// A progress tick opens the throttle window, a second inside it must be
	// suppressed, then the terminal — all well inside a second.
	reportProgress(t, ri, 1, 10)
	reportProgress(t, ri, 2, 10)
	_, err := m.Send(context.Background(), ri, CancelIndex())
	require.NoError(t, err)

	var progress int
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-stream:
			ev, ok := e.(events.IndexEvent)
			if !ok || ev.Repo != ri.Name() {
				continue
			}
			if ev.State == events.IndexStateIndexing && ev.Total == 10 {
				progress++
				continue
			}
			if ev.State == events.IndexStateError {
				require.Equal(t, "indexing cancelled", ev.Reason)
				// And the suppressed tick really was suppressed: exactly one
				// progress frame, not two. Without this the test would pass
				// against no throttle at all.
				require.Equal(t, 1, progress, "progress frames in one throttle window")
				return
			}
		case <-deadline:
			t.Fatal("the terminal frame never arrived")
		}
	}
}

// A MOUNT ANNOUNCES ITS ENTRY INTO INDEXING (issue #570). A fresh machine's
// first publish must emit an `indexing` event even though its initial derived
// status already says indexing: the machine seeds its last-published snapshot
// as the zero value, so entering indexing is a change like any other.
//
// The index job is held at its hook for the whole check, so no progress frame
// and no terminal frame can exist: the only way an `indexing` frame reaches the
// stream while the job is parked is the entry event. (Before the fix the
// frame was a throttled progress frame, present only when the driver handled
// indexProgressed before indexDone, which is what flaked
// TestRepoIndexEvents_StartupHealEmitsIndexingThenReady under load.)
func TestRepoIndexEvents_MountAnnouncesEntryIntoIndexing(t *testing.T) {
	g := newGate(StageIndex, "index-job")
	m := newTestManager(t)
	m.deps.Machine.Hook = g.hook
	t.Cleanup(g.open)
	require.NoError(t, m.Start())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := m.RepoEventHub().Subscribe(ctx)

	created := make(chan error, 1)
	go func() {
		_, err := m.Create(context.Background(), CreateSpec{Name: testRepoName, Mode: "preset"}, nil)
		created <- err
	}()
	g.waitArrived(t)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-stream:
			ev, ok := e.(events.IndexEvent)
			if !ok || ev.Repo != testRepoName {
				continue
			}
			require.Equal(t, events.IndexStateIndexing, ev.State, "the first index event of a mount")
			require.Zero(t, ev.Done)
			require.Zero(t, ev.Total)
			g.open()
			require.NoError(t, <-created)
			return
		case <-deadline:
			t.Fatal("the index job is parked and no entry `indexing` event was published")
		}
	}
}
