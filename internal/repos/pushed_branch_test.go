package repos

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

func TestIsPushedBranch(t *testing.T) {
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/host-0a1b2c3d"))

	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "r", AgentBranch: "agent/host-0a1b2c3d", Svc: svc})
	for branch, want := range map[string]bool{
		"agent/peer-99887766":     true,  // a peer's pushed branch
		"agent/host-0a1b2c3d":     false, // this instance's agent branch
		"agent/oldname-0a1b2c3d":  false, // own branch from before a rename
		"main":                    false, // the upstream (and not agent/*)
		"exp/try":                 false, // an experiment
		"feature/x":               false, // anything outside agent/
		"agent/peer-without-fp8x": true,  // no fp8 suffix: still a peer branch
	} {
		require.Equal(t, want, ri.IsPushedBranch(branch), branch)
	}

	sub := NewTestInstanceWithDeps(TestInstanceConfig{Name: "s", Subscribed: true, ReadBranch: "main", Svc: svc})
	require.False(t, sub.IsPushedBranch("agent/peer-99887766"), "a subscription has no pushed branches")
}
