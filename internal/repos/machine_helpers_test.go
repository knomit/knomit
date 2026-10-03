package repos

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Test helpers for the lifecycle machine. None of them sleeps to synchronise:
// they wait on Watch, on a hook's arrival channel, or on Done.

// testCrashBackoff is the crash backoff the package's test managers use, so a
// restart through the crash path (restartServe) costs milliseconds.
const testCrashBackoff = 10 * time.Millisecond

// waitIndexSettled waits, through Watch, until ri's index has left
// "indexing", and returns the status it settled in. It replaces the old
// open→index-ready contract: the index job always runs in the background.
func waitIndexSettled(t *testing.T, ri *RepoInstance) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for tr := range ri.Watch(ctx) {
		if tr.Status.Index.State != IndexStateIndexing {
			return tr.Status
		}
	}
	t.Fatalf("index of %s did not settle; last status %+v", ri.Name(), ri.Status())
	return Status{}
}

// waitStatus waits, through Watch, until pred holds for ri's status.
func waitStatus(t *testing.T, ri *RepoInstance, msg string, pred func(Status) bool) Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for tr := range ri.Watch(ctx) {
		if pred(tr.Status) {
			return tr.Status
		}
	}
	t.Fatalf("%s: status never matched; last %+v", msg, ri.Status())
	return Status{}
}

// restartServe restarts ri's Serve stage through the machine's own crash path
// — exactly what a panicking worker triggers: the life is cancelled, its
// workers drain (the trigger dispatcher flushes its buffer on the way out),
// and after the crash backoff the stage is re-entered. It returns once the new
// Serve life is up. A test uses it where it used to stop the dispatcher by
// hand.
func restartServe(t *testing.T, ri *RepoInstance) {
	t.Helper()
	gen := ri.Status().gens[StageServe]
	require.NotZero(t, gen, "Serve is not entered")
	ri.machine.post(crashed{stage: StageServe, gen: gen})
	waitStatus(t, ri, "Serve re-entered", func(s Status) bool {
		return s.gens[StageServe] != 0 && s.gens[StageServe] != gen && s.Serve.Running
	})
}

// mountAs mounts the registered repo uid under name — what a boot does for a
// registry row, Populate a no-op — and registers it in m's maps.
func mountAs(t *testing.T, m *Manager, name, uid string, origin *Origin) *RepoInstance {
	t.Helper()
	ri, err := m.mountExisting(name, uid, origin)
	require.NoError(t, err)
	m.Set(name, ri)
	return ri
}

// swapStore installs the database at tmp over ri's through the SwapStore
// event, keeping ri's stored origin, and returns the event's reply error.
func swapStore(m *Manager, ri *RepoInstance, tmp string) error {
	_, err := m.Send(context.Background(), ri, SwapStore(SwapSpec{TempDB: tmp}))
	return err
}

// gate holds one hook point (a stage's "enter", or the index job's
// "index-job") until the test opens it. Like every hook it selects on the
// life's ctx, so a held stage still unmounts.
type gate struct {
	stage StageID
	point string

	mu       sync.Mutex
	hits     int
	arrived  chan struct{}
	arriveMu sync.Once
	release  chan struct{}
	openOnce sync.Once
}

func newGate(stage StageID, point string) *gate {
	return &gate{stage: stage, point: point, arrived: make(chan struct{}), release: make(chan struct{})}
}

// hook is an Options.Hook. Only the gate's own point blocks.
func (g *gate) hook(stage StageID, point string, ctx context.Context) {
	if stage != g.stage || point != g.point {
		return
	}
	g.mu.Lock()
	g.hits++
	g.mu.Unlock()
	g.arriveMu.Do(func() { close(g.arrived) })
	select {
	case <-g.release:
	case <-ctx.Done():
	}
}

// waitArrived waits until the held point has been reached.
func (g *gate) waitArrived(t *testing.T) {
	t.Helper()
	select {
	case <-g.arrived:
	case <-time.After(30 * time.Second):
		t.Fatalf("the hook point %s/%s was never reached", stageName(g.stage), g.point)
	}
}

// open lets every holder through, now and later. Idempotent.
func (g *gate) open() { g.openOnce.Do(func() { close(g.release) }) }

func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// hooks chains several Options.Hooks.
func hooks(hs ...func(StageID, string, context.Context)) func(StageID, string, context.Context) {
	return func(s StageID, p string, ctx context.Context) {
		for _, h := range hs {
			h(s, p, ctx)
		}
	}
}
