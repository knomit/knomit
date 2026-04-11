package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStoryboard_RepoBootAndVerify asserts that NewStoryboard creates a temp
// home, Repo("alpha") boots a manager and registers a repo, and the teardown
// auto-verify runs without error.
func TestStoryboard_RepoBootAndVerify(t *testing.T) {
	t.Log("Scenario: NewStoryboard → Repo(alpha) → Branch(agent/test) → teardown auto-verifies clean")
	sb := NewStoryboard(t)
	r := sb.Repo("alpha")
	require.NotNil(t, r)
	require.Equal(t, "alpha", r.Name())
	require.NotNil(t, r.Instance())

	br := r.Branch("agent/test")
	require.NotNil(t, br)
	require.Equal(t, "agent/test", br.Name())

	// Explicit verify to exercise MustVerify on top of the teardown path.
	r.MustVerify()
}

// TestStoryboard_MultipleRepos asserts that two Repo() calls in the same
// Storyboard create independent repos in independent subdirectories of the
// tempdir, and teardown verifies both.
func TestStoryboard_MultipleRepos(t *testing.T) {
	t.Log("Scenario: two repos in one Storyboard, each gets its own manager and its own Verify")
	sb := NewStoryboard(t)
	a := sb.Repo("alpha")
	b := sb.Repo("beta")

	require.NotEqual(t, a.Instance(), b.Instance(), "distinct RepoInstances")
	a.MustVerify()
	b.MustVerify()
}

// TestStoryboard_RepoReturnsCachedHandle asserts that Repo("x") called twice
// returns the same RepoHandle (idempotent).
func TestStoryboard_RepoReturnsCachedHandle(t *testing.T) {
	t.Log("Scenario: Repo(\"x\") called twice returns the same handle (no second manager boot)")
	sb := NewStoryboard(t)
	r1 := sb.Repo("alpha")
	r2 := sb.Repo("alpha")
	require.Same(t, r1, r2, "Repo should return cached handle on second call")
}
