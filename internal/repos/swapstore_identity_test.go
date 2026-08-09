package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// A disjoint-history connect swaps the store, and with it the repo's root
// commit. The registry must follow — otherwise repo_id points at a knowledge
// base this repo no longer holds.
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

	require.NoError(t, m.SwapStore(ri, otherPath))

	after, _, err := m.reg.Get(uid)
	require.NoError(t, err)
	require.Equal(t, otherID, after.RepoID, "identity follows the store")
}

// A FAILED swap must bring the repo back fully wired, origin included.
//
// rewireStore re-applies what store.Open does not restore, and its doc read as
// a complete enumeration while silently omitting the injected origin. On the
// success path that never showed: the connect handler re-injects afterwards.
// On the failure paths — copy, reopen, git-open — SwapStore goes
// reattach(reopenLocal()) and the handler returns without re-injecting. The
// repo came back live with origin == nil, and the reconcile loop then hit
// `if remote == nil { return }` and exited with no log line at all: a repo that
// looks healthy and has quietly stopped syncing.
func TestSwapStore_FailedSwapKeepsTheInjectedOrigin(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")

	const originURL = "https://origin.test/kb.git"
	require.NoError(t, m.Origins().Set(ri.UID(), Origin{URL: originURL, Branch: "main"}))
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(&store.Origin{URL: originURL, Branch: "main"})
	}))

	// Fail the swap at the copy, the first of the three recovery paths.
	err := m.SwapStore(ri, filepath.Join(t.TempDir(), "never-written.db"))
	require.Error(t, err)

	var remote *store.Remote
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		remote, _ = svc.Remote().GetRemote("origin")
	}))
	require.NotNil(t, remote,
		"a failed swap must not come back with origin == nil; the reconcile loop exits silently on that")
	require.Equal(t, originURL, remote.URL)
}
