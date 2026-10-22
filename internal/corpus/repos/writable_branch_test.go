package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWritableBranch_V1Classification(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{
		Name:        "test",
		AgentBranch: "agent/test",
	})

	require.True(t, ri.WritableBranch("agent/test"), "own agent branch is writable")
	require.False(t, ri.WritableBranch("main"), "consensus branch is never writable")
	require.False(t, ri.WritableBranch("agent/other-machine"), "foreign agent branches are never writable")
	require.False(t, ri.WritableBranch(""), "empty branch is not writable")
}

// A subscription has NO agent branch, so no branch is writable — including
// the branch it reads and the empty name.
func TestWritableBranch_SubscriptionWritesNothing(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "sub", Subscribed: true, ReadBranch: "main",
	})
	require.True(t, ri.Subscribed())
	require.Equal(t, "", ri.AgentBranch())
	require.Equal(t, "main", ri.ReadBranch())
	for _, b := range []string{"main", "agent/sub", ""} {
		require.False(t, ri.WritableBranch(b), "branch %q must not be writable on a subscription", b)
	}
}

func TestReadBranch_DefaultsToAgentBranch(t *testing.T) {
	ri := NewTestInstanceWithDeps(TestInstanceConfig{Name: "w", AgentBranch: "agent/test"})
	require.False(t, ri.Subscribed())
	require.Equal(t, "agent/test", ri.ReadBranch())
}
