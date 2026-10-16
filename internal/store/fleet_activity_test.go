package store

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// FleetMainLog reports, newest first, the first-parent commits that add the
// root certificate, change the host record (with the URLs it leaves) or
// change a member record (before and after, read as the loader reads them),
// and nothing else; the walk stops after maxCommits commits.
// SABOTAGE: diff each commit against the TIP instead of its first parent ->
// the record rows multiply -> red; ignore maxCommits -> the short walk has 5
// rows -> red.
func TestFleetMainLog_RootHostAndRecords(t *testing.T) {
	h := newFleetPushHost(t, nil)
	const x = "kb/members/x/1.md"
	k := namedSigner(t, "x").PublicKey()
	h.mainWrite("artifacts/fleet/root.crt", "root")
	h.mainWrite(x, memberFile("x", fact.MemberActive, k))
	h.mainWrite("README.md", "nothing the page shows")
	h.mainWrite(fact.FleetHostPath, "# knomit fleet host v1\nurl: https://h.example/git/fleet\n")
	h.mainWrite(x, memberFile("x", fact.MemberLeft, k))

	log, err := h.svc.FleetMainLog("main", "artifacts/fleet/root.crt", 100, 100)
	require.NoError(t, err)
	require.Len(t, log, 5, "left, host, joined, root, and the init's boss record: %+v", log)
	require.Len(t, log[0].Records, 1)
	require.Equal(t, "x", log[0].Records[0].Agent)
	require.Equal(t, fact.MemberActive, log[0].Records[0].Before.State)
	require.Equal(t, fact.MemberLeft, log[0].Records[0].After.State)
	require.True(t, log[1].HostChanged)
	require.Equal(t, []string{"https://h.example/git/fleet"}, log[1].Host)
	require.Empty(t, log[1].Records)
	require.Nil(t, log[2].Records[0].Before)
	require.Equal(t, fact.MemberActive, log[2].Records[0].After.State)
	require.True(t, log[3].RootAdded)
	require.False(t, log[2].RootAdded || log[1].RootAdded || log[0].RootAdded)
	require.Equal(t, "boss", log[4].Records[0].Agent, "the root commit adds boss's record")
	for _, c := range log {
		require.Empty(t, c.MergedTip, "no merges here")
		require.NotEqual(t, plumbing.ZeroHash.String(), c.Hash)
	}

	short, err := h.svc.FleetMainLog("main", "artifacts/fleet/root.crt", 2, 100)
	require.NoError(t, err)
	require.Len(t, short, 2, "the walk stops after 2 commits")
	few, err := h.svc.FleetMainLog("main", "artifacts/fleet/root.crt", 100, 3)
	require.NoError(t, err)
	require.Len(t, few, 3, "and once it has maxEntries")
	require.Equal(t, log[:3], few, "the newest ones")
}
