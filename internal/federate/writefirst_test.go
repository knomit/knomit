package federate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// TestWriteFirstWinners pins the cross-mount dedupe contract every lens union
// read shares: a repo-relative path present on more than one mount resolves to a
// single winner — the WRITE mount first, then read mounts in binding order —
// regardless of the write mount's positional index in the target list.
func TestWriteFirstWinners(t *testing.T) {
	inst := func(name string) *repos.RepoInstance {
		return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: name, AgentBranch: "agent/test"})
	}
	a, b, c := inst("a-write"), inst("b-read"), inst("c-read")

	// Write mount (a) is deliberately NOT first in the target order, to prove the
	// winner is chosen by write-first priority, not list position.
	targets := []Target{
		{RT: repos.ReadTarget{RI: b}}, // 0
		{RT: repos.ReadTarget{RI: a}}, // 1 (write)
		{RT: repos.ReadTarget{RI: c}}, // 2
	}
	lists := [][]string{
		{"kb/shared.md", "kb/bc.md", "kb/b-only.md"}, // b
		{"kb/shared.md", "kb/a-only.md"},             // a (write)
		{"kb/shared.md", "kb/bc.md", "kb/c-only.md"}, // c
	}

	winner := WriteFirstWinners(targets, a, lists, func(s string) string { return s })

	// Shared across all three → the write mount (index 1) wins.
	require.Equal(t, 1, winner["kb/shared.md"], "write mount must win a path it also holds")
	// Present only on the two read mounts (b, c) → earlier-in-binding-order b wins.
	require.Equal(t, 0, winner["kb/bc.md"], "among reads, binding order (b before c) breaks the tie")
	// Unique paths map to their own mount.
	require.Equal(t, 1, winner["kb/a-only.md"])
	require.Equal(t, 0, winner["kb/b-only.md"])
	require.Equal(t, 2, winner["kb/c-only.md"])
	require.Len(t, winner, 5, "exactly the distinct paths, deduped")
}
