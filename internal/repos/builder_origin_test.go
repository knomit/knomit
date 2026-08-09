package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The injected origin must reach the store BEFORE openGit, because
// rehydrateUpstreamMain and the fetch refspec both read it there. A repo whose
// origin tracks "master" must not fall back to the literal "main".
//
// The origin is injected into a store exactly once, at open time (openStore,
// before openGit) — a repo already running does not pick up a later
// control.db write on its own (Task 10 wires live updates). So this writes
// the origin, then re-opens through openOne exactly as a reboot would via
// Start/openRegistered — m.origins.Get followed by m.Add — and checks the
// freshly-opened store.
func TestOpenOne_InjectedOriginDrivesUpstream(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NotEmpty(t, uid)

	require.NoError(t, m.origins.Set(uid, Origin{URL: "https://x.test/kb.git", Branch: "master"}))

	m.Remove("core")
	origin, err := m.origins.Get(uid)
	require.NoError(t, err)
	require.NoError(t, m.Add("core", uid, m.RepoPath(uid), origin))
	ri = m.Get("core")
	require.NotNil(t, ri)

	svc := testService(t, ri)
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "master", got.Branch)
}
