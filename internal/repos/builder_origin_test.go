package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The injected origin must reach the store BEFORE openGit, because
// rehydrateUpstreamMain and the fetch refspec both read it there. A repo whose
// origin tracks "master" must not fall back to the literal "main".
func TestOpenOne_InjectedOriginDrivesUpstream(t *testing.T) {
	t.Skip("uid is assigned in Task 6")

	m := newTestManager(t)
	require.NoError(t, m.Start())
	ri := createRepo(t, m, "core")
	uid := ri.UID()
	require.NotEmpty(t, uid)

	require.NoError(t, m.origins.Set(uid, Origin{URL: "https://x.test/kb.git", Branch: "master"}))

	svc := testService(t, ri)
	got, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "master", got.Branch)
}
