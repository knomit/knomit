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
