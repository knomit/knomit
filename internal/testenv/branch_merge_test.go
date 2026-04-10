package testenv

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestMergeFrom_FastForward asserts that MergeFrom produces a fast-forward
// when the receiver is an ancestor of src: src's HEAD becomes the new HEAD,
// no merge commit is created.
func TestMergeFrom_FastForward(t *testing.T) {
	t.Log("Scenario: main has no diverging work, feature writes X, main.MergeFrom(feature) fast-forwards to feature's HEAD")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	feature := repo.BranchFrom("feature", "main")

	snap := feature.Write("kb/x.md", Fact("x"), "add x on feature")

	merge := main.MergeFrom(feature, store.StrategyLocalWins)
	require.Equal(t, snap.Commit, merge.Commit, "fast-forward should make main HEAD == feature HEAD")

	// Main should now see the fact.
	main.Head().Fact("kb/x.md").MustExist()
}

// TestMergeFrom_ThreeWayDisjointLocalWins asserts that disjoint changes
// from both branches are combined under LocalWins.
func TestMergeFrom_ThreeWayDisjointLocalWins(t *testing.T) {
	t.Log("Scenario: main adds A, feature adds B, merge LocalWins yields main with both facts")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	feature := repo.BranchFrom("feature", "main")

	main.Write("kb/a.md", Fact("a"), "add a")
	feature.Write("kb/b.md", Fact("b"), "add b")

	main.MergeFrom(feature, store.StrategyLocalWins)

	head := main.Head()
	head.Fact("kb/a.md").MustExist()
	head.Fact("kb/b.md").MustExist()
}

// TestMergeFrom_ThreeWayConflictLocalWins asserts that when both branches
// modify the same path, LocalWins keeps the receiver's version.
func TestMergeFrom_ThreeWayConflictLocalWins(t *testing.T) {
	t.Log("Scenario: both branches modify kb/x.md; main.MergeFrom(feature, LocalWins) keeps main's version")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	main.Write("kb/x.md", Fact("x").Body("original"), "add x")
	feature := repo.BranchFrom("feature", "main")

	main.Update("kb/x.md", Fact("x").Body("main version"), "main update")
	feature.Update("kb/x.md", Fact("x").Body("feature version"), "feature update")

	main.MergeFrom(feature, store.StrategyLocalWins)
	main.Head().Fact("kb/x.md").Body().MustContain("main version")
}

// TestMergeFrom_ThreeWayConflictRemoteWins asserts that the same conflict
// resolved with RemoteWins takes src's version.
func TestMergeFrom_ThreeWayConflictRemoteWins(t *testing.T) {
	t.Log("Scenario: both branches modify kb/x.md; main.MergeFrom(feature, RemoteWins) takes feature's version")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	main.Write("kb/x.md", Fact("x").Body("original"), "add x")
	feature := repo.BranchFrom("feature", "main")

	main.Update("kb/x.md", Fact("x").Body("main version"), "main update")
	feature.Update("kb/x.md", Fact("x").Body("feature version"), "feature update")

	main.MergeFrom(feature, store.StrategyRemoteWins)
	main.Head().Fact("kb/x.md").Body().MustContain("feature version")
}

// TestMergeFrom_AlreadyMergedNoOp asserts that merging a branch that is
// already a strict ancestor of the receiver is a no-op.
func TestMergeFrom_AlreadyMergedNoOp(t *testing.T) {
	t.Log("Scenario: feature is ancestor of main (main advanced after branching), merge is a no-op")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	feature := repo.BranchFrom("feature", "main")
	main.Write("kb/a.md", Fact("a"), "add a on main")
	headBefore := main.Head().Commit

	main.MergeFrom(feature, store.StrategyLocalWins)
	require.Equal(t, headBefore, main.Head().Commit, "main HEAD must not move")
}

// TestMergeFrom_SnapshotCaptured asserts that MergeFrom returns a
// Snapshot whose commit matches the repo's HEAD after the merge and
// that the snapshot is pushed onto the receiver branch's snapshot stack.
func TestMergeFrom_SnapshotCaptured(t *testing.T) {
	t.Log("Scenario: MergeFrom returns a snapshot pointing at the new HEAD and appends it to the stack")
	sb := NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	feature := repo.BranchFrom("feature", "main")

	main.Write("kb/a.md", Fact("a"), "add a")
	feature.Write("kb/b.md", Fact("b"), "add b")

	stackBefore := len(main.SnapshotsForTest())
	snap := main.MergeFrom(feature, store.StrategyLocalWins)
	require.NotEmpty(t, snap.Commit)
	require.Equal(t, "C2", snap.Name, "merge should push as the next C<N> snapshot on main")
	require.Len(t, main.SnapshotsForTest(), stackBefore+1)
}
