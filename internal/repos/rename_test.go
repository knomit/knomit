package repos

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// TestRenameRepo_RekeysWithoutClosingTheStore is the whole point of choosing
// in-place re-key over Remove+Add: the SAME instance pointer must survive, with
// its store still open, because a rename changes a display string and must not
// cost a store close, an SSE drop and an index re-warm.
func TestRenameRepo_RekeysWithoutClosingTheStore(t *testing.T) {
	m := newTestManager(t)
	before := bootNamedRepo(t, m, "alpha")
	uid := before.UID()

	require.NoError(t, m.RenameRepo("alpha", "beta"))

	require.Nil(t, m.Get("alpha"), "the old name must no longer resolve")
	after := m.Get("beta")
	require.NotNil(t, after, "the new name must resolve")
	require.Same(t, before, after, "same instance — the store was never closed")
	require.Equal(t, "beta", after.Name(), "the instance reports its new name")
	require.Equal(t, uid, after.UID(), "uid is identity and never changes")
	require.Same(t, before, m.GetByUID(uid), "byUID still points at the live instance")

	// The store is still usable — a REAL read, not just a successful Acquire.
	// This is what Remove+Add would have broken (a closed store fails here).
	require.NoError(t, after.WithRead(func(svc *store.Service) {
		_, err := svc.Branches().HeadCommit(context.Background(), after.AgentBranch())
		require.NoError(t, err)
	}))
}

func TestRenameRepo_RejectsInvalidName(t *testing.T) {
	m := newTestManager(t)
	bootNamedRepo(t, m, "alpha")
	require.ErrorIs(t, m.RenameRepo("alpha", "Has Capitals"), ErrInvalidName)
	require.ErrorIs(t, m.RenameRepo("alpha", ""), ErrInvalidName)
	require.NotNil(t, m.Get("alpha"), "a rejected rename leaves the repo alone")
}

func TestRenameRepo_RejectsNameHeldByAnotherActiveRepo(t *testing.T) {
	m := newTestManager(t)
	bootNamedRepo(t, m, "alpha")
	createRepo(t, m, "beta")

	require.ErrorIs(t, m.RenameRepo("alpha", "beta"), ErrRepoExists)
	require.NotNil(t, m.Get("alpha"))
	require.NotNil(t, m.Get("beta"))
}

func TestRenameRepo_UnknownRepo(t *testing.T) {
	m := newTestManager(t)
	bootNamedRepo(t, m, "alpha")
	require.ErrorIs(t, m.RenameRepo("ghost", "beta"), ErrRepoNotFound)
}

// Renaming to the current name is a successful no-op, not a self-collision.
func TestRenameRepo_SameNameIsNoOp(t *testing.T) {
	m := newTestManager(t)
	before := bootNamedRepo(t, m, "alpha")

	require.NoError(t, m.RenameRepo("alpha", "alpha"))
	require.Same(t, before, m.Get("alpha"))
}

// A lens may hold newName even though no repo does — repos and lenses share
// one namespace (gotcha M-1). Same guard Create and Restore run; RenameRepo
// must run it too, and until this test existed nothing pinned that it did.
func TestRenameRepo_RejectsNameHeldByLens(t *testing.T) {
	m := newTestManager(t)
	bootNamedRepo(t, m, "alpha")
	writer := createRepo(t, m, "writer")

	_, err := m.CreateLens(context.Background(), Lens{Name: "beta", WriteUID: writer.UID()})
	require.NoError(t, err)

	require.ErrorIs(t, m.RenameRepo("alpha", "beta"), ErrRepoNameConflictsLens)
	require.NotNil(t, m.Get("alpha"), "a rejected rename leaves the repo alone")
}

// A concurrent Create/Restore already bringing newName into the active map
// must block RenameRepo from claiming it too — reserveNameAndOrigin is the
// shared gate, and until this test existed nothing pinned that RenameRepo
// actually goes through it. Held directly here to stand in for the concurrent
// operation, the same technique TestRestore_HonorsInFlightReservation uses.
func TestRenameRepo_RejectsWhenTargetNameInFlight(t *testing.T) {
	m := newTestManager(t)
	bootNamedRepo(t, m, "alpha")

	release, err := m.reserveNameAndOrigin("beta", "")
	require.NoError(t, err)
	defer release()

	require.ErrorIs(t, m.RenameRepo("alpha", "beta"), ErrCreateInFlight)
	require.NotNil(t, m.Get("alpha"), "a rejected rename leaves the repo alone")
}

// TestRenameRepo_PersistsAcrossRestart pins the durable half: the UPDATE to
// control.db, not just the in-memory map move. An implementation that only
// re-keyed m.repos and never called reg.Rename would pass every other test in
// this file (they all inspect the live Manager) but fail this one, because a
// fresh boot re-reads names from the registry, not from the map that just got
// torn down by m1.Close().
func TestRenameRepo_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	boot := func() *Manager {
		m := New(context.Background(), Deps{
			Cfg:                   config.Config{Home: dir},
			AgentBranch:           "machine/test",
			DisableBackgroundSync: true,
		})
		require.NoError(t, m.Start())
		return m
	}

	m1 := boot()
	createRepo(t, m1, "alpha")
	require.NoError(t, m1.RenameRepo("alpha", "beta"))
	require.NoError(t, m1.Close())

	m2 := boot()
	t.Cleanup(func() { _ = m2.Close() })
	require.Nil(t, m2.Get("alpha"), "the old name must not resurrect on reboot")
	require.NotNil(t, m2.Get("beta"),
		"the rename must have reached the registry row, not only the in-memory map")
}

// TestRenameRepo_ConcurrentDifferentTargets_ExactlyOneWins reproduces Race A
// from the review: two RenameRepo calls on the SAME oldName to DIFFERENT
// newNames reserve different names, so reserveNameAndOrigin alone excludes
// neither from the other. Before the revalidate-under-the-lock fix, both
// would pass every check and both would land in the map (repos["beta"] AND
// repos["gamma"] pointing at the same instance) — this assertion is what
// catches that: exactly one of the two names may resolve afterwards, because
// the map holds one instance under one name, not two.
func TestRenameRepo_ConcurrentDifferentTargets_ExactlyOneWins(t *testing.T) {
	m := newTestManager(t)
	ri := bootNamedRepo(t, m, "alpha")

	start := make(chan struct{})
	var wg sync.WaitGroup
	var errBeta, errGamma error

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errBeta = m.RenameRepo("alpha", "beta")
	}()
	go func() {
		defer wg.Done()
		<-start
		errGamma = m.RenameRepo("alpha", "gamma")
	}()
	close(start)
	wg.Wait()

	betaResolves := m.Get("beta") != nil
	gammaResolves := m.Get("gamma") != nil
	require.NotEqual(t, betaResolves, gammaResolves,
		"exactly one of beta/gamma must resolve, got beta=%v gamma=%v (errBeta=%v errGamma=%v)",
		betaResolves, gammaResolves, errBeta, errGamma)

	// Whichever name won, it is still the SAME instance, and byUID agrees.
	require.Same(t, ri, m.GetByUID(ri.UID()))

	// The durable half must agree with the live map. This is the assertion an
	// UNCONDITIONAL compensation fails: the loser only reaches the revalidate
	// AFTER the winner's map swap has already completed (it takes m.mu after
	// the winner releases it), so an unconditional revert-to-oldName runs
	// chronologically last and clobbers the winner's registry row — leaving
	// the registry holding oldName while the live map/instance report the
	// winner's name, undetected until the next restart re-reads the registry
	// and disagrees with the state that existed right before it.
	winnerName := "beta"
	if gammaResolves {
		winnerName = "gamma"
	}
	rec, ok, err := m.reg.Get(ri.UID())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, winnerName, rec.Name,
		"the registry row must hold the same name the live map resolves, not oldName")
}

// TestRenameRepo_ConcurrentReadersDuringRenameLoop is the -race regression for
// the reason ri.name became an atomic.Pointer in the first place: RenameRepo
// writes it on a live instance while unsynchronised readers — mostly
// Get/Names/ForEach callers and Name() itself — read it concurrently. This
// test does not assert particular values (a renamer racing readers makes any
// single observation meaningless); its only job is to give `go test -race`
// something to catch if setName, Get, Names, or ForEach ever stop agreeing on
// how they're synchronised.
func TestRenameRepo_ConcurrentReadersDuringRenameLoop(t *testing.T) {
	m := newTestManager(t)
	ri := bootNamedRepo(t, m, "alpha")

	const iterations = 200
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		cur, other := "alpha", "beta"
		for i := 0; i < iterations; i++ {
			require.NoError(t, m.RenameRepo(cur, other))
			cur, other = other, cur
		}
	}()

	const readers = 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = ri.Name()
				_ = m.Get("alpha")
				_ = m.Get("beta")
				_ = m.Names()
			}
		}()
	}

	wg.Wait()
}
