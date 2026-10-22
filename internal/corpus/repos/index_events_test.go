package repos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
	events := m.RepoEvents(ctx)

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
		case e := <-events:
			ev, ok := e.(IndexEvent)
			if !ok || ev.Repo != ri.Name() {
				continue
			}
			if ev.State == IndexStateIndexing && ev.Total == 10 {
				progress++
				continue
			}
			if ev.State == IndexStateError {
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
