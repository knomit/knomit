// G9 — Squash-merge regression. The scenario that motivated the
// merge-based rework: an agent commits N facts to its branch, the remote
// absorbs them via a squash-merge to main (no parent linkage), the local
// sync tick must fast-forward the agent to main with zero new agent
// commits and zero branch_facts churn.
//
// Under the previous rebase-based reconcile this scenario produced N
// orphan commits per sync tick (tree-level no-ops that still rewrote
// branch_facts / branch_commits). This test pins the fix.
package storytests

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/test/testenv"
)

// TestReconcile_G9_SquashMergeFastForwardsAgent: 30 local facts on the
// agent, remote squash-merges them into main, local sync fast-forwards
// the agent to main with zero new commits and identical branch_facts
// row count to pre-sync.
func TestReconcile_G9_SquashMergeFastForwardsAgent(t *testing.T) {
	t.Log("G9: 30 facts → squash-merge to main → local sync FF agent; no orphan commits")
	sb := testenv.NewStoryboard(t)
	remote := sb.BareRemote("origin")
	remote.WriteMain("kb/seed.md", testenv.Fact("seed"), "seed")

	a := sb.Repo("a").Connect(remote)
	agent := a.Branch("agent/test")
	require.True(t, agent.HasFile("kb/seed.md"), "agent inherited seed")

	// Agent commits N facts.
	const N = 30
	for i := 0; i < N; i++ {
		path := fmt.Sprintf("kb/local-%03d.md", i)
		agent.Write(path, testenv.Fact("local").Body(fmt.Sprintf("body-%d", i)), fmt.Sprintf("local %d", i))
	}
	agent.Push()

	preSyncAgentTip := agent.HeadCommit()
	preSyncCommitCount := agent.CommitCount()
	preSyncFactCount := agent.FactCount()

	// Remote absorbs all N facts via squash-merge: origin/main now has a
	// single new commit containing every file, but origin/agent's chain is
	// NOT in main's ancestry.
	remote.SquashMergeIntoMain("agent/test", "squash-merge agent into main")

	// Local sync. With the merge-based reconcile, this must:
	//   - fast-forward local main to the new origin/main tip (the squash commit)
	//   - agent is ancestor of main? No — agent's chain is not in main.
	//   - main is ancestor of agent? No — main's squash commit is not in agent.
	//   - But agent's tree == main's tree (both contain the same N+1 facts).
	//   - Three-way merge yields a tree identical to dst (agent) → reported
	//     as "noop", agent ref unchanged.
	//
	// Net agent state: ref unchanged, watermark advanced to new local main.
	syncResult := agent.Sync()

	// Main fast-forwarded.
	require.Equal(t, store.ModeFF, syncResult.Main.Mode, "main must fast-forward to new origin/main")

	// Agent: this is the critical assertion. With the OLD design, agent
	// would be replayed with N orphan commits. With the NEW design, the
	// merge is a tree-level no-op and agent ref is unchanged.
	require.Equal(t, store.ModeNoop, syncResult.Agent.Mode,
		"squash-merged content is identical → agent reconcile is no-op (no merge commit, no rebase)")

	// Agent ref and counts unchanged.
	require.Equal(t, preSyncAgentTip, agent.HeadCommit(), "agent tip unchanged")
	require.Equal(t, preSyncCommitCount, agent.CommitCount(), "no new agent commits")
	require.Equal(t, preSyncFactCount, agent.FactCount(), "fact count unchanged")
}
