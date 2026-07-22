package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/federate"
	"knomit/internal/repos"
)

// TestKeepWinners_DropsShadowedCrossMountCopy pins the fix for the knomit_query
// double-listing bug: when a fact path exists on both the write mount and a read
// mount (a fork whose upstream is read-mounted shares fact UUIDs), the fused
// order carries BOTH refs. keepWinners must drop the read-mount (shadowed) ref
// and keep the write-mount copy, so the union matches the web /facts + /search
// views instead of emitting the fact twice (once bare, once kb://-qualified).
func TestKeepWinners_DropsShadowedCrossMountCopy(t *testing.T) {
	inst := func(name string) *repos.RepoInstance {
		return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: name, AgentBranch: "agent/test"})
	}
	write, read := inst("write"), inst("read")

	targets := []federate.Target{
		{RT: repos.ReadTarget{RI: write}}, // mount 0 (write)
		{RT: repos.ReadTarget{RI: read}},  // mount 1 (read)
	}
	lists := [][]string{
		{"kb/shared.md"},               // write mount
		{"kb/shared.md", "kb/read.md"}, // read mount: shadows shared, plus its own
	}
	// The fused order as FuseRRF([1,2]) would produce: both copies of the shared
	// path (ranks interleaved) plus the read-only fact.
	order := federate.FuseRRF([]int{1, 2}) // [{0,0},{1,0},{1,1}]

	got := keepWinners(order, targets, write, lists, func(s string) string { return s })

	// {0,0} = write's shared copy (kept), {1,0} = read's shadowed copy (dropped),
	// {1,1} = read's own fact (kept). Original relative order preserved.
	require.Equal(t, []federate.MountRef{{Mount: 0, Rank: 0}, {Mount: 1, Rank: 1}}, got)
}
