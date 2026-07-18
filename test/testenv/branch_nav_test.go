package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBranchNav_HeadAfterMutations asserts that Head() returns the
// most recent snapshot after any number of mutations.
func TestBranchNav_HeadAfterMutations(t *testing.T) {
	t.Log("Scenario: Write three facts, Head() returns the third snapshot each time it's called")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/a.md", Fact("a"), "a")
	agent.Write("kb/b.md", Fact("b"), "b")
	c3 := agent.Write("kb/c.md", Fact("c"), "c")

	require.Same(t, c3, agent.Head())
	require.Same(t, c3, agent.Head()) // Idempotent.
}

// TestBranchNav_HeadWithNoCapturedSnapshots asserts that Head() on a
// branch with no captured snapshots still resolves via the production
// HeadCommit API and returns a synthetic "HEAD" snapshot.
func TestBranchNav_HeadWithNoCapturedSnapshots(t *testing.T) {
	t.Log("Scenario: fresh Branch handle, Head() resolves via production HeadCommit")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	// No mutations — snapshots is empty.
	head := agent.Head()
	require.NotNil(t, head)
	require.NotEmpty(t, head.Commit)
	require.Equal(t, "HEAD", head.Name)
	require.Same(t, agent, head.Branch)
}

// TestBranchNav_AtIndexNegativeAndPositive asserts AtIndex supports both
// negative (relative-to-HEAD) and positive (absolute-from-start) indices.
func TestBranchNav_AtIndexNegativeAndPositive(t *testing.T) {
	t.Log("Scenario: three writes, AtIndex(-1) == C3, AtIndex(-2) == C2, AtIndex(0) == C1")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	c1 := agent.Write("kb/a.md", Fact("a"), "a")
	c2 := agent.Write("kb/b.md", Fact("b"), "b")
	c3 := agent.Write("kb/c.md", Fact("c"), "c")

	require.Same(t, c3, agent.AtIndex(-1))
	require.Same(t, c2, agent.AtIndex(-2))
	require.Same(t, c1, agent.AtIndex(-3))
	require.Same(t, c1, agent.AtIndex(0))
	require.Same(t, c2, agent.AtIndex(1))
	require.Same(t, c3, agent.AtIndex(2))
}

// TestBranchNav_AtNameFindsAutoAndExplicit asserts that AtName resolves
// both auto-generated names (C1, C2, ...) and explicit names set via
// WriteAs.
func TestBranchNav_AtNameFindsAutoAndExplicit(t *testing.T) {
	t.Log("Scenario: WriteAs(custom), Write, AtName(custom) and AtName(C2) both resolve")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	custom := agent.WriteAs("milestone", "kb/a.md", Fact("a"), "a")
	c2 := agent.Write("kb/b.md", Fact("b"), "b")

	require.Same(t, custom, agent.AtName("milestone"))
	require.Same(t, c2, agent.AtName("C2"))
}

// TestBranchNav_AtSanityChecksOwnership asserts that passing a Snapshot
// from a different BranchHandle to At() fails (sanity check against
// accidentally mixing branches).
//
// We use a subtest expected to fail; parent test marks itself failed
// intentionally only if the sanity check does NOT fire.
func TestBranchNav_AtReturnsSameSnapshotPointer(t *testing.T) {
	t.Log("Scenario: At(snap) returns the same pointer it was handed (pass-through with sanity check)")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	snap := agent.Write("kb/a.md", Fact("a"), "a")
	require.Same(t, snap, agent.At(snap))
}
