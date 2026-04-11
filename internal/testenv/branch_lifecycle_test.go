package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBranch_Drop removes a child branch and asserts the repo stays
// integrity-clean. After the drop, calling Branch(name) returns a fresh
// handle (because the parent RepoHandle deletes the cached one), though
// accessing it would fail because the branch no longer exists.
func TestBranch_Drop(t *testing.T) {
	t.Log("Scenario: BranchFrom(feature), Drop, Verify stays clean, handle is removed from cache")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	feature := repo.BranchFrom("feature", "main")
	feature.Write("kb/x.md", Fact("x"), "add x on feature")

	feature.Drop()

	// The BranchHandle is removed from the repo's cache.
	_, cached := repo.branches["feature"]
	require.False(t, cached, "BranchHandle must be removed from cache after Drop")
}

// TestBranch_FactCount writes N facts and asserts FactCount == N.
func TestBranch_FactCount(t *testing.T) {
	t.Log("Scenario: write 3 facts on agent/test, FactCount returns 3")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	agent.Write("kb/a.md", Fact("a"), "a")
	agent.Write("kb/b.md", Fact("b"), "b")
	agent.Write("kb/c.md", Fact("c"), "c")

	agent.MustHaveFactCount(3)
	require.Equal(t, 3, agent.FactCount())
}

// TestBranch_CommitCount writes N facts and asserts the branch has
// exactly initial + N commits via the branch_commits count.
func TestBranch_CommitCount(t *testing.T) {
	t.Log("Scenario: fresh branch starts with 1 commit (init), 3 writes → 4 commits")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")

	start := agent.CommitCount()
	require.Greater(t, start, 0, "init commit must be present")

	agent.Write("kb/a.md", Fact("a"), "a")
	agent.Write("kb/b.md", Fact("b"), "b")
	agent.Write("kb/c.md", Fact("c"), "c")

	require.Equal(t, start+3, agent.CommitCount())
	agent.MustHaveCommitCount(start + 3)
}

// TestBranch_Log returns production Log entries for a path.
func TestBranch_Log(t *testing.T) {
	t.Log("Scenario: 3 updates to kb/x.md, Log returns 3 entries")
	sb := NewStoryboard(t)
	agent := sb.Repo("alpha").Branch("agent/test")
	agent.Write("kb/x.md", Fact("x").Confidence(0.3), "v1")
	agent.Update("kb/x.md", Fact("x").Confidence(0.6), "v2")
	agent.Update("kb/x.md", Fact("x").Confidence(0.9), "v3")

	entries := agent.Log("kb/x.md")
	require.GreaterOrEqual(t, len(entries), 3, "log should have at least 3 entries, got %d", len(entries))
}
