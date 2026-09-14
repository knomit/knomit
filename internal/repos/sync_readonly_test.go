package repos

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPushAllowed(t *testing.T) {
	require.True(t, pushAllowed(false, "agent/x"))
	require.False(t, pushAllowed(true, "agent/x"), "instance read-only is pull-only")
	require.False(t, pushAllowed(false, ""), "a repo with no agent branch has nothing to push")
}
