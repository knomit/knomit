package httpx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The gate's MCP exemption matches the mount's whole shape: a fact path that
// ends in "mcp" is not the mount.
func TestIsRepoMCPMount(t *testing.T) {
	require.True(t, isRepoMCPMount("/api/v1/repos/fleet/branches/agent:x/mcp", "fleet"))
	require.True(t, isRepoMCPMount("/repos/fleet/branches/agent%2Fx/mcp/", "fleet"))
	require.False(t, isRepoMCPMount("/repos/fleet/branches/agent:x/facts/kb/mcp", "fleet"))
	require.False(t, isRepoMCPMount("/repos/fleet/rename", "fleet"))
	require.False(t, isRepoMCPMount("/repos/other/branches/b/mcp", "fleet"))
}
