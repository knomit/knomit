package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBranch_WriteReturnsSnapshotAndAutoVerifies asserts that Write on an
// existing branch produces a Snapshot with a non-empty commit hash, the
// snapshot is appended to the branch's stack with an auto-generated name,
// and the auto-verify on the repo stays clean (since Write goes through
// the real WriteFact path that maintains branch_facts / commit_log / etc).
func TestBranch_WriteReturnsSnapshotAndAutoVerifies(t *testing.T) {
	t.Log("Scenario: Write fact → Snapshot has commit hash, stack has one entry named C1, auto-verify clean")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	snap := agent.Write("kb/x.md", Fact("x").Confidence(0.7), "add x")
	require.NotNil(t, snap)
	require.NotEmpty(t, snap.Commit)
	require.Equal(t, "C1", snap.Name)
	require.Same(t, agent, snap.Branch)

	stack := agent.SnapshotsForTest()
	require.Len(t, stack, 1)
	require.Same(t, snap, stack[0])
}

// TestBranch_WriteSequenceAssignsSequentialNames asserts that multiple
// mutations auto-generate C1, C2, C3 snapshot names in order.
func TestBranch_WriteSequenceAssignsSequentialNames(t *testing.T) {
	t.Log("Scenario: Write, Update, Update produces C1, C2, C3")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	c1 := agent.Write("kb/a.md", Fact("a"), "add a")
	c2 := agent.Update("kb/a.md", Fact("a").Confidence(0.8), "revise a")
	c3 := agent.Update("kb/a.md", Fact("a").Confidence(0.9), "revise a again")

	require.Equal(t, "C1", c1.Name)
	require.Equal(t, "C2", c2.Name)
	require.Equal(t, "C3", c3.Name)
	require.NotEqual(t, c1.Commit, c2.Commit)
	require.NotEqual(t, c2.Commit, c3.Commit)
	require.Len(t, agent.SnapshotsForTest(), 3)
}

// TestBranch_WriteAsAndDeleteAsRespectExplicitNames asserts that explicit
// names via WriteAs/DeleteAs override auto-generation.
func TestBranch_WriteAsAndDeleteAsRespectExplicitNames(t *testing.T) {
	t.Log("Scenario: WriteAs(before), Delete, DeleteAs(after) produces snapshots named explicitly")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	before := agent.WriteAs("before", "kb/x.md", Fact("x"), "write x")
	require.Equal(t, "before", before.Name)

	// Auto-generated name between two explicit ones still increments the counter.
	mid := agent.Update("kb/x.md", Fact("x").Confidence(0.9), "revise")
	require.Equal(t, "C2", mid.Name)

	after := agent.DeleteAs("after", "kb/x.md", "retract x")
	require.Equal(t, "after", after.Name)
	require.NotEqual(t, mid.Commit, after.Commit)
}

// TestBranch_DeleteRemovesFactAndStaysIntegral asserts that deleting a
// fact produces a valid commit and the repo remains integrity-clean —
// the auto-verify catches any residue in branch_facts / facts_vec / etc.
func TestBranch_DeleteRemovesFactAndStaysIntegral(t *testing.T) {
	t.Log("Scenario: Write then Delete, auto-verify stays clean across both ops")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	agent.Write("kb/x.md", Fact("x"), "add x")
	del := agent.Delete("kb/x.md", "retract")
	require.NotEmpty(t, del.Commit)
	require.Len(t, agent.SnapshotsForTest(), 2)
	// Teardown auto-verify will fire again at test end.
}
