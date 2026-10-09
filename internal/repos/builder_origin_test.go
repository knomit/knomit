package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The injected origin must reach the store BEFORE OpenRepo, because
// rehydrating the upstream and the fetch refspec both read it there. A repo
// whose origin tracks "master" must not fall back to the literal "main".
//
// The Open stage injects the origin control.db holds — a repo already running
// does not pick up a bare control.db write on its own (AttachOrigin is the
// event for that). So this writes the origin, then re-mounts exactly as a
// reboot would via Start/openRegistered, and checks the freshly-opened store.
func TestOpenStage_InjectedOriginDrivesUpstream(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NotEmpty(t, uid)

	require.NoError(t, m.origins.Set(uid, Origin{URL: "https://x.test/kb.git", Branch: "master"}))

	m.Remove("core")
	origin, err := m.origins.Get(uid)
	require.NoError(t, err)
	ri = mountAs(t, m, "core", uid, origin)

	svc := testService(t, ri)
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "master", got.Branch)
}
