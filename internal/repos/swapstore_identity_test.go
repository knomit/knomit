package repos

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// A disjoint-history connect swaps the store, and with it the repo's root
// commit. The registry must follow — otherwise repo_id points at a knowledge
// base this repo no longer holds. Identify, re-entered by the swap's walk, is
// the one site that records it.
func TestSwapStore_RecordsNewRepoID(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()

	before, _, err := m.reg.Get(uid)
	require.NoError(t, err)
	require.NotEmpty(t, before.RepoID)

	// A second, independently-created repo has a different root commit; use its
	// database as the swap source.
	other := createRepo(t, m, "other")
	otherID := other.ID()
	require.NotEqual(t, before.RepoID, otherID)
	otherPath := m.RepoPath(other.UID())
	_, err = m.Archive("other")
	require.NoError(t, err)

	require.NoError(t, swapStore(m, ri, otherPath))

	after, _, err := m.reg.Get(uid)
	require.NoError(t, err)
	require.Equal(t, otherID, after.RepoID, "identity follows the store")
	require.Equal(t, otherID, ri.ID())
}

// A FAILED swap must bring the repo back fully wired, origin included.
//
// The install fails here at its first step (the backup cannot be written: a
// directory sits where it would go), after every stage was exited. The walk
// still runs — an apply error is the reply, never a reason to leave stages
// exited — so the Open stage reopens the previous store through its one
// wiring function, injecting the origin control.db still holds. A repo that
// came back with origin == nil would look healthy and have quietly stopped
// syncing.
func TestSwapStore_FailedSwapKeepsTheInjectedOrigin(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	other := createRepo(t, m, "other")
	otherPath := m.RepoPath(other.UID())
	_, err := m.Archive("other")
	require.NoError(t, err)

	const originURL = "https://origin.test/kb.git"
	require.NoError(t, m.Origins().Set(ri.UID(), Origin{URL: originURL, Branch: "main"}))
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(&store.Origin{URL: originURL, Branch: "main"})
	}))
	idBefore := ri.ID()

	require.NoError(t, os.Mkdir(m.RepoPath(ri.UID())+".bak", 0o755))
	_, err = m.Send(context.Background(), ri, SwapStore(SwapSpec{
		TempDB: otherPath, Origin: OriginSpec{URL: "https://elsewhere.test/kb.git", Branch: "main"},
	}))
	require.Error(t, err)
	require.ErrorContains(t, err, "swap failed")
	require.Equal(t, "ready", ri.Status().Stage, "the walk ran after the failed apply")

	var remote *store.Remote
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		remote, _ = svc.Remote().GetRemote("origin")
	}))
	require.NotNil(t, remote,
		"a failed swap must not come back with origin == nil; the repo would stop syncing silently")
	require.Equal(t, originURL, remote.URL, "the previous origin, not the one the failed swap carried")
	require.Equal(t, idBefore, ri.ID(), "the previous store is back")
}
