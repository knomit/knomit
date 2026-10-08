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

// subscriptionStore opens a bare remote and initialises a subscription store
// the way initSubscribe does, with its registry row, stopping short of the
// mount so a caller can choose the origin row it registers.
//
// Deps are built inline rather than through newLifecycleManagerWithRoot because
// these tests need BOTH a LocalOriginRoot (the file:// remote) and a
// Synchronous machine (no sync loop racing the assertions).
func subscriptionStore(t *testing.T, uid string) (m *Manager, url, dbPath, upstream string) {
	t.Helper()
	root := t.TempDir()
	m = New(context.Background(), Deps{
		Cfg:         config.Config{Home: t.TempDir(), LocalOriginRoot: root},
		AgentBranch: "machine/test",
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	url = seedBareRemote(t, filepath.Join(root, "remote.git"))

	require.NoError(t, m.Repos().Insert(RepoRecord{UID: uid, Name: uid, State: StateActive, Profile: ProfileCode, CreatedAt: 1}))
	dbPath = m.RepoPath(uid)
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	svc, err := store.Open(dbPath)
	require.NoError(t, err)
	svc.SetNetworkTimeout(m.deps.Cfg.Git.NetworkTimeout)
	upstream, err = svc.InitSubscription(context.Background(), url, nil, "", nil)
	require.NoError(t, err)
	require.NoError(t, svc.Close())
	return m, url, dbPath, upstream
}

// mountSubscription records origin as uid's origin row and mounts the repo
// the way a boot does.
func mountSubscription(m *Manager, uid string, origin Origin) (*RepoInstance, error) {
	if err := m.Origins().Set(uid, origin); err != nil {
		return nil, err
	}
	return m.mountExisting(uid, uid, &origin)
}

// buildSubscription mounts a subscription store with a well-formed
// subscribe-mode origin. Returns the instance and the upstream.
func buildSubscription(t *testing.T) (*Manager, *RepoInstance, string) {
	t.Helper()
	m, url, _, upstream := subscriptionStore(t, "uid-sub")
	ri, err := mountSubscription(m, "uid-sub", Origin{URL: url, Branch: upstream, Mode: OriginModeSubscribe})
	require.NoError(t, err)
	m.Set("uid-sub", ri)
	return m, ri, upstream
}

// Mounting a repo whose origin is in subscribe mode builds an agent-less
// instance: the read branch is the upstream, the store is read-only, and the
// index heal covers the upstream alone.
func TestMount_SubscriptionBuildsAgentlessReadOnlyInstance(t *testing.T) {
	t.Parallel()
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

	// The index heals the upstream and reaches ready.
	require.Equal(t, IndexStateReady, waitIndexSettled(t, ri).Index.State)
}

// A store swap must not resurrect write access. readOnly lives on the store's
// repoHandler, which store.Open rebuilds from disk, so it survives a reopen
// ONLY because the Open stage applies it on every Enter — the one wiring
// function for the first open and every reopen alike. Without it a
// subscription silently becomes writable: no error, no log, and every other
// assertion in the test above still passes.
func TestSwapStore_SubscriptionStaysReadOnly(t *testing.T) {
	t.Parallel()
	m, ri, upstream := buildSubscription(t)
	waitIndexSettled(t, ri)

	require.NoError(t, ri.WithRead(func(s *store.Service) { require.NoError(t, s.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, m.RepoPath(ri.UID()), tmp)
	require.NoError(t, swapStore(m, ri, tmp), "the swap reopens the store through the Open stage")

	require.NoError(t, ri.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteRootFile(context.Background(), upstream, "README.md", "x", "m", "updated")
		require.ErrorIs(t, werr, store.ErrRepoReadOnly,
			"a reopened subscription store must still refuse authored writes")
	}))
}

// A subscribe-mode origin with no recorded upstream is refused at Open rather
// than producing a repo with no read branch — and therefore no ontology, no
// identity and no index. Create persists the RESOLVED upstream, so this state
// means a corrupted or hand-edited origin row; the stage's message is what an
// operator will see.
func TestMount_SubscriptionWithoutUpstreamIsRefused(t *testing.T) {
	t.Parallel()
	m, url, _, upstream := subscriptionStore(t, "uid-sub2")

	// Origins.Set refuses an empty branch, so the corruption is written
	// underneath it, the way a hand edit would.
	require.NoError(t, m.Origins().Set("uid-sub2", Origin{URL: url, Branch: upstream, Mode: OriginModeSubscribe}))
	_, err := m.Repos().DB().Exec(`UPDATE repo_origins SET branch = '' WHERE repo_uid = ?`, "uid-sub2")
	require.NoError(t, err)
	ri, err := m.mountExisting("uid-sub2", "uid-sub2", &Origin{URL: url, Mode: OriginModeSubscribe})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorContains(t, err, "no upstream branch recorded")
	require.Nil(t, ri, "a refused mount must not return an instance")

	// The recovery an operator performs: fix the origin row, mount the SAME
	// database again, get a working subscription.
	fixed, err := mountSubscription(m, "uid-sub2", Origin{URL: url, Branch: upstream, Mode: OriginModeSubscribe})
	require.NoError(t, err, "mounting after the refusal must work")
	m.Set("uid-sub2", fixed)
	require.Equal(t, upstream, fixed.ReadBranch())
}
