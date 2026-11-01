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

// The gate's FactSet-close exemption matches only the factset resource itself
// (its DELETE is checked by the caller): not the export below it, not a fact
// path that ends in search/factset, not another repository's.
func TestIsRepoFactSetResource(t *testing.T) {
	require.True(t, isRepoFactSetResource("/api/v1/repos/fleet/branches/agent:x/search/factset", "fleet"))
	require.True(t, isRepoFactSetResource("/repos/fleet/branches/agent%2Fx/search/factset", "fleet"))
	require.False(t, isRepoFactSetResource("/repos/fleet/branches/agent:x/search/factset/", "fleet"))
	require.False(t, isRepoFactSetResource("/repos/fleet/branches/agent:x/search/factset/export", "fleet"))
	require.False(t, isRepoFactSetResource("/repos/fleet/branches/agent:x/facts/kb/search/factset", "fleet"))
	require.False(t, isRepoFactSetResource("/repos/fleet/branches/agent:x/search/factsetx", "fleet"))
	require.False(t, isRepoFactSetResource("/repos/fleet/branches//search/factset", "fleet"))
	require.False(t, isRepoFactSetResource("/repos/other/branches/b/search/factset", "fleet"))
}
