package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
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
