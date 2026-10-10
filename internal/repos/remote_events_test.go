package repos

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// remoteEventLog records the RemoteEvents a repo's hub publishes, and lets a
// test assert that NOTHING was published by a round: it publishes its own
// marker on the same hub and waits for it. goob delivers to one subscriber in
// publish order, so once the marker is seen every event published before it
// has been seen too. That is what makes "no frame" an observation rather than
// a sleep.
type remoteEventLog struct {
	mu      sync.Mutex
	remote  []RemoteEvent
	markers map[string]bool
	hub     *RepoEventHub
	seq     int
}

func subscribeRemoteEvents(t *testing.T, ri *RepoInstance) *remoteEventLog {
	t.Helper()
	require.NotNil(t, ri.repoEventHub, "precondition: the repo is mounted by a Manager with a repo-event hub")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	events := ri.repoEventHub.Subscribe(ctx)
	l := &remoteEventLog{markers: map[string]bool{}, hub: ri.repoEventHub}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-events:
				l.mu.Lock()
				switch ev := e.(type) {
				case RemoteEvent:
					l.remote = append(l.remote, ev)
				case IndexEvent:
					l.markers[ev.Repo] = true
				}
				l.mu.Unlock()
			}
		}
	}()
	return l
}

// settle publishes a marker and returns every RemoteEvent seen before it.
func (l *remoteEventLog) settle(t *testing.T) []RemoteEvent {
	t.Helper()
	l.seq++
	marker := fmt.Sprintf("marker-%d", l.seq)
	l.hub.publish(IndexEvent{Repo: marker})
	require.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.markers[marker]
	}, 10*time.Second, 2*time.Millisecond, "marker %s never arrived", marker)
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RemoteEvent(nil), l.remote...)
}

func persistedHealth(t *testing.T, ri *RepoInstance) (string, string) {
	t.Helper()
	r, err := testService(t, ri).Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, r)
	return RemoteHealth(r)
}

// The `remote` repo-event is an EDGE: published when a persisted status column
// changes value, silent in steady state. Driven through the real loop against
// a real file:// origin; the sync half fails through an auth-resolution
// failure, which the loop persists (RecordSyncError) exactly like a fetch
// error.
//
// Sabotage: publish on every tick (drop the prev == after check in
// publishRemoteEdge) → the healthy round 1 and the second failing round each
// emit a frame → red. Publish from the step's result instead of the re-read
// → the event still matches here, but see the restart test.
func TestRemoteEvents_EdgesOnly(t *testing.T) {
	ri, _, root := newOriginTriggerRepo(t)
	// The clone's own Synchronous Sync round already ran both halves.
	s, p := persistedHealth(t, ri)
	require.Equal(t, [2]string{"ok", "ok"}, [2]string{s, p}, "precondition: a healthy origin")
	log := subscribeRemoteEvents(t, ri)

	l := startBreakerLoop(t, ri, root, nil) // round 1: healthy, nothing changed
	require.Empty(t, log.settle(t), "a healthy tick over a healthy row publishes nothing")

	l.round(t, ri) // round 2: the control — a SECOND ok tick
	require.Empty(t, log.settle(t), "steady state is silent")

	l.authFail.Store(true)
	l.round(t, ri) // round 3: ok → error on the sync half
	require.Equal(t, []RemoteEvent{{Repo: ri.Name(), Sync: "error", Push: "ok"}}, log.settle(t),
		"exactly one frame, carrying the whole row: sync error, push untouched")

	l.round(t, ri) // round 4: still failing
	require.Len(t, log.settle(t), 1, "a second failing tick is not an edge")

	l.authFail.Store(false)
	l.round(t, ri) // round 5: error → ok
	got := log.settle(t)
	require.Len(t, got, 2)
	require.Equal(t, RemoteEvent{Repo: ri.Name(), Sync: "ok", Push: "ok"}, got[1], "the recovery edge")

	l.round(t, ri) // round 6: healthy again
	require.Len(t, log.settle(t), 2, "and silent again")
}

// THE RESTART CASE (kb invariant a8d54411): the server stopped while the row
// said "error", so the fresh loop's in-memory failure counters are zero. Its
// first healthy tick is still the recovery edge, because the prior value is
// the persisted row.
//
// Sabotage: compute the edge from syncFails/pushFails (zero after a restart)
// → no frame → red.
func TestRemoteEvents_RecoveryEdgeAfterRestart(t *testing.T) {
	ri, _, root := newOriginTriggerRepo(t)
	require.NoError(t, testService(t, ri).Remote().RecordSyncError("origin", "dial tcp: lookup example: no such host"))
	s, _ := persistedHealth(t, ri)
	require.Equal(t, "error", s, "precondition: the persisted row says error before the loop starts")
	log := subscribeRemoteEvents(t, ri)

	startBreakerLoop(t, ri, root, nil) // a fresh loop: counters at zero
	require.Equal(t, []RemoteEvent{{Repo: ri.Name(), Sync: "ok", Push: "ok"}}, log.settle(t))
}

// Pure: the wire vocabulary of one status column. NULL is "never ran", not
// "ok" — a dot must not claim health for a half that has not been tried.
func TestRemoteStatusValue(t *testing.T) {
	ok, bad := "ok", "error"
	require.Equal(t, "", RemoteStatusValue(nil))
	require.Equal(t, "ok", RemoteStatusValue(&ok))
	require.Equal(t, "error", RemoteStatusValue(&bad))
	s, p := RemoteHealth(&store.Remote{LastStatus: &ok})
	require.Equal(t, [2]string{"ok", ""}, [2]string{s, p})
}
