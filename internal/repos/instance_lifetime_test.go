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
	"knomit/internal/store"
)

// newLifetimeTestManager boots a Manager in a temp home and creates the test
// repo in it. DisableBackgroundSync keeps construction synchronous and free of
// network activity.
func newLifetimeTestManager(t *testing.T) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: t.TempDir()},
		AgentBranch:           "agent/test",
		DisableBackgroundSync: true,
	})
	t.Cleanup(func() { _ = m.Close() })
	bootRepo(t, m)
	return m
}

// TestAcquire_AfterClose_ReturnsErrRepoClosed: once teardown ran, Acquire and
// WithRead must fail with ErrRepoClosed instead of handing out a closed store.
func TestAcquire_AfterClose_ReturnsErrRepoClosed(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	ri.shutdown()

	_, _, err := ri.Acquire()
	require.ErrorIs(t, err, ErrRepoClosed)

	called := false
	err = ri.WithRead(func(*store.Service) { called = true })
	require.ErrorIs(t, err, ErrRepoClosed)
	require.False(t, called, "WithRead must not invoke fn on a closed instance")

	_, err = ri.Verify(context.Background(), store.VerifyOpts{})
	require.ErrorIs(t, err, ErrRepoClosed)
}

// TestClose_WaitsForInFlightAcquire: teardown must drain an outstanding
// Acquire before closing the store — the in-flight user finishes its store
// call on an open service, never observing "database is closed".
func TestClose_WaitsForInFlightAcquire(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	svc, release, err := ri.Acquire()
	require.NoError(t, err)

	closed := make(chan struct{})
	go func() {
		ri.shutdown()
		close(closed)
	}()

	// The close must not complete while we hold the acquisition.
	select {
	case <-closed:
		t.Fatal("shutdown completed while an Acquire was outstanding")
	case <-time.After(100 * time.Millisecond):
	}

	// The store must still be fully usable while held.
	_, err = svc.Branches().HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err, "store must stay open while acquired")

	release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete after release")
	}
}

// TestWithRead_ConcurrentWithClose_NeverSeesClosedStore hammers WithRead from
// multiple goroutines while teardown runs. Every fn invocation must see a
// working store; once closed, callers must get ErrRepoClosed — never a
// "database is closed" SQL error or a panic. Run with -race.
func TestWithRead_ConcurrentWithClose_NeverSeesClosedStore(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)

	var sqlErrs atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				err := ri.WithRead(func(svc *store.Service) {
					if _, herr := svc.Branches().HeadCommit(context.Background(), "agent/test"); herr != nil {
						sqlErrs.Add(1)
					}
				})
				if err != nil {
					require.ErrorIs(t, err, ErrRepoClosed)
					return
				}
			}
		}()
	}

	time.Sleep(20 * time.Millisecond)
	ri.shutdown()
	close(stop)
	wg.Wait()
	require.Zero(t, sqlErrs.Load(), "no acquired reader may ever observe a closed/failed store")
}

// TestSwapStore_DrainsInFlightUsers: an in-memory SwapStore must wait for
// outstanding acquisitions of the old generation before closing it, and new
// acquisitions after the swap must see the new service.
func TestSwapStore_DrainsInFlightUsers(t *testing.T) {
	m := newLifetimeTestManager(t)
	ri := m.Get(testRepoName)
	require.NotNil(t, ri)
	// Force the in-memory (pointer-swap) path.
	ri.dbPath = ""

	oldSvc, release, err := ri.Acquire()
	require.NoError(t, err)

	// Build a second store to swap in.
	tempDB := filepath.Join(t.TempDir(), "swap.db")
	seed, err := store.Open(tempDB)
	require.NoError(t, err)
	require.NoError(t, seed.InitRepo(map[string]string{}, "agent/test"))
	seed.Close()

	swapped := make(chan error, 1)
	go func() { swapped <- m.SwapStore(ri, tempDB) }()

	// The swap must not finish while the old generation is held.
	select {
	case <-swapped:
		t.Fatal("SwapStore completed while an Acquire on the old store was outstanding")
	case <-time.After(100 * time.Millisecond):
	}

	// Old service must still work while held.
	_, err = oldSvc.Branches().HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err, "old store must stay open until released")

	release()
	select {
	case err := <-swapped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("SwapStore did not complete after release")
	}

	// New acquisitions see the swapped-in service, and it works.
	newSvc, release2, err := ri.Acquire()
	require.NoError(t, err)
	defer release2()
	require.NotSame(t, oldSvc, newSvc, "post-swap Acquire must return the new service")
	_, err = newSvc.Branches().HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err)
}
