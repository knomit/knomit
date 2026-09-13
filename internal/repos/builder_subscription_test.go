package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// buildSubscription opens a bare remote, initialises a subscription store the
// way Task 7's initSubscribe will, and registers it through openOne with a
// subscribe-mode origin. Returns the instance and the resolved upstream.
// Deps are built inline rather than through newLifecycleManagerWithRoot because
// this test needs BOTH a LocalOriginRoot (the file:// remote) and
// DisableBackgroundSync (so the index heal and activation run inline and
// IndexStatus is settled when openOne returns). Neither existing helper sets
// both — newLifecycleManagerWithRoot sets only the first, newTestManager only
// the second — and several tests in this package construct Deps this way.
func buildSubscription(t *testing.T) (*Manager, *RepoInstance, string) {
	t.Helper()
	root := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:                   config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	dbPath := m.RepoPath("uid-sub")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
	upstream, err := svc.InitSubscription(url, nil, "")
	require.NoError(t, err)
	require.NoError(t, svc.Close())

	ri, err := m.openOne("sub", "uid-sub", dbPath, &Origin{URL: url, Branch: upstream, Mode: OriginModeSubscribe})
	require.NoError(t, err)
	t.Cleanup(func() { ri.Close() })
	return m, ri, upstream
}

// Opening a repo whose origin is in subscribe mode builds an agent-less
// instance: the read branch is the upstream, the store is read-only, and the
// index heal covers the upstream alone.
func TestOpenOne_SubscriptionBuildsAgentlessReadOnlyInstance(t *testing.T) {
	_, ri, upstream := buildSubscription(t)

	// The THREE representations of "this is a subscription" asserted together,
	// so they cannot diverge silently: the flag, the absent agent branch, and
	// the write classification that keys on the branch rather than the flag.
	require.True(t, ri.Subscribed())
	require.Equal(t, "", ri.AgentBranch())
	for _, b := range []string{upstream, "agent/anything", ""} {
		require.False(t, ri.WritableBranch(b), "branch %q must not be writable on a subscription", b)
	}

	require.Equal(t, upstream, ri.ReadBranch())
	require.NotEmpty(t, ri.ID(), "identity resolves on the read branch")
	require.NotNil(t, ri.Ontology(), "the ontology is read from the upstream")

	// The store refuses authored writes.
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteRootFile(context.Background(), upstream, "README.md", "x", "m", "updated")
		require.ErrorIs(t, werr, store.ErrRepoReadOnly)
	}))

	// The index is ready and holds the upstream (DisableBackgroundSync heals inline).
	state, _, _ := ri.IndexStatus()
	require.Equal(t, "ready", state)
}

// A store swap must not resurrect write access. readOnly lives on the store's
// repoHandler, which store.Open rebuilds from disk, so it survives ONLY because
// rewireStore re-applies it — the same reason SetOntologyRoot and
// SetNetworkTimeout are in that checklist. Without the re-apply a subscription
// silently becomes writable: no error, no log, and every other assertion in the
// test above still passes.
//
// Driven through the COPY-FAILURE path (a temp db that was never written),
// which is the one the reopen-rewire invariant names: SwapStore goes
// reattach(reopenLocal()) and the caller returns without re-wiring anything
// itself.
func TestSwapStore_SubscriptionStaysReadOnly(t *testing.T) {
	m, ri, upstream := buildSubscription(t)

	require.Error(t, m.SwapStore(ri, filepath.Join(t.TempDir(), "never-written.db")),
		"the swap must fail so the reattach/reopen recovery path runs")

	require.NoError(t, ri.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteRootFile(context.Background(), upstream, "README.md", "x", "m", "updated")
		require.ErrorIs(t, werr, store.ErrRepoReadOnly,
			"a reopened subscription store must still refuse authored writes")
	}))
}
