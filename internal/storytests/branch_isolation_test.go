// Category A — Branch isolation. These tests assert that operations on
// one branch never perturb another. This is the foundational invariant
// from working.md: "I can create a new branch, then delete it and the
// other branches are not going to be affected."
//
// Every test in this file ends with the Storyboard teardown auto-verify,
// which double-checks integrity across every tracked repo.
package storytests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/testenv"
)

// ── A1 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_WriteOnOneBranchInvisibleToOther asserts that
// writing a fact on main does not cause the sibling branch agent/test to
// see it. This is the simplest form of branch isolation — writes to one
// branch must not leak into another.
func TestBranchIsolation_WriteOnOneBranchInvisibleToOther(t *testing.T) {
	t.Log("A1: write to main; agent/test (sibling) does not see the file at HEAD")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	agent := repo.Branch("agent/test")

	main.Write("kb/onmain.md", testenv.Fact("m"), "add on main")

	agent.Head().Fact("kb/onmain.md").MustNotExist()
	main.Head().Fact("kb/onmain.md").MustExist()
}

// ── A2 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_WriteOnChildInvisibleToParent asserts that when a
// child branch writes a new fact, the parent's HEAD does not move and
// its tree does not contain the new fact.
func TestBranchIsolation_WriteOnChildInvisibleToParent(t *testing.T) {
	t.Log("A2: branch feature from main, write on feature; main's HEAD unchanged")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	headBefore := main.Head().Commit

	feature := repo.BranchFrom("feature", "main")
	feature.Write("kb/featureonly.md", testenv.Fact("fo"), "add feature-only fact")

	require.Equal(t, headBefore, main.Head().Commit, "main HEAD must not move")
	main.Head().Fact("kb/featureonly.md").MustNotExist()
}

// ── A3 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_DropBranchDoesNotAffectOthers creates three
// branches from main, writes on each, drops one, and asserts the other
// two have byte-identical state before and after the drop. Verify
// stays clean throughout.
func TestBranchIsolation_DropBranchDoesNotAffectOthers(t *testing.T) {
	t.Log("A3: 3 branches from main, write on each, drop branch 2, branches 1 and 3 unchanged")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")

	one := repo.BranchFrom("branch1", "main")
	two := repo.BranchFrom("branch2", "main")
	three := repo.BranchFrom("branch3", "main")
	one.Write("kb/one.md", testenv.Fact("one"), "add one")
	two.Write("kb/two.md", testenv.Fact("two"), "add two")
	three.Write("kb/three.md", testenv.Fact("three"), "add three")

	oneHeadBefore := one.Head().Commit
	threeHeadBefore := three.Head().Commit

	two.Drop()

	require.Equal(t, oneHeadBefore, one.Head().Commit, "branch1 HEAD unchanged")
	require.Equal(t, threeHeadBefore, three.Head().Commit, "branch3 HEAD unchanged")
	one.Head().Fact("kb/one.md").MustExist()
	three.Head().Fact("kb/three.md").MustExist()
}

// ── A4 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_ChildCapturesParentAtCreation asserts that a child
// branch inherits its parent's content at the moment of creation, and
// that subsequent writes on the parent do NOT retroactively appear on
// the child. Pure snapshot semantics.
func TestBranchIsolation_ChildCapturesParentAtCreation(t *testing.T) {
	t.Log("A4: write X on parent, branch child, write Y on parent; child has X but not Y")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	main.Write("kb/x.md", testenv.Fact("x"), "add x")

	feature := repo.BranchFrom("feature", "main")

	main.Write("kb/y.md", testenv.Fact("y"), "add y later on main")

	feature.Head().Fact("kb/x.md").MustExist()
	feature.Head().Fact("kb/y.md").MustNotExist()
	main.Head().Fact("kb/x.md").MustExist()
	main.Head().Fact("kb/y.md").MustExist()
}

// ── A5 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_ManyBranchesFromSameBase creates 10 children from
// one base, each writes a distinct fact, and asserts per-branch
// isolation across the full matrix.
func TestBranchIsolation_ManyBranchesFromSameBase(t *testing.T) {
	t.Log("A5: 10 branches from main, each writes a distinct fact, no cross-contamination")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")

	branches := make([]*testenv.BranchHandle, 10)
	for i := range 10 {
		name := branchName(i)
		branches[i] = repo.BranchFrom(name, "main")
		branches[i].Write(factPath(i), testenv.Fact(factTitle(i)), "add")
	}

	// For every branch, assert it has ITS OWN fact and none of the others.
	for i, br := range branches {
		br.Head().Fact(factPath(i)).MustExist()
		for j := range branches {
			if j == i {
				continue
			}
			br.Head().Fact(factPath(j)).MustNotExist()
		}
	}
}

// ── A6 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_DeletedFactOnOneBranchStillPresentOnOther asserts
// that when a fact is created on a parent, inherited by a child, and
// then deleted on the child, the parent still has the fact at HEAD.
// Shared content on creation, divergent lifecycles afterward.
func TestBranchIsolation_DeletedFactOnOneBranchStillPresentOnOther(t *testing.T) {
	t.Log("A6: write X on main, branch feature, delete X on feature; main still has X")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")
	main.Write("kb/x.md", testenv.Fact("x"), "add x on main")

	feature := repo.BranchFrom("feature", "main")
	feature.Delete("kb/x.md", "retract x on feature")

	feature.Head().Fact("kb/x.md").MustNotExist()
	main.Head().Fact("kb/x.md").MustExist()
}

// ── A7 ────────────────────────────────────────────────────────────────────

// TestBranchIsolation_ChildInheritsParentHistory asserts the git-
// semantics invariant from working.md: a newly-created child branch's
// commit set equals the parent's at creation time. Subsequent parent
// writes do NOT appear on the child (pure snapshot, not live view).
//
// Verifies the fix in commit e07c514 where CreateBranch was updated to
// also clone branch_facts in addition to branch_commits, so the child
// sees every parent-inherited fact via the same per-branch view rows.
func TestBranchIsolation_ChildInheritsParentHistory(t *testing.T) {
	t.Log("A7: 5 writes on parent, BranchFrom; child sees all 5 facts and has matching commit count")
	sb := testenv.NewStoryboard(t)
	repo := sb.Repo("alpha")
	main := repo.Branch("main")

	// Five writes on main before branching.
	for i := range 5 {
		main.Write(factPath(i), testenv.Fact(factTitle(i)), "add")
	}

	mainCommitsBefore := main.CommitCount()
	feature := repo.BranchFrom("feature", "main")

	// Child sees every parent fact.
	for i := range 5 {
		feature.Head().Fact(factPath(i)).MustExist()
	}

	// Child's commit count matches parent's at the moment of branching.
	require.Equal(t, mainCommitsBefore, feature.CommitCount(),
		"child must inherit parent's full commit count")

	// Write a new fact on the parent AFTER branching — child must NOT see it.
	main.Write("kb/after.md", testenv.Fact("after"), "added after branching")
	feature.Head().Fact("kb/after.md").MustNotExist()
	main.Head().Fact("kb/after.md").MustExist()
}

// ── helpers ───────────────────────────────────────────────────────────────

func branchName(i int) string { return "feature" + itoa(i) }
func factPath(i int) string   { return "kb/item" + itoa(i) + ".md" }
func factTitle(i int) string  { return "item " + itoa(i) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	idx := len(buf)
	for n > 0 {
		idx--
		buf[idx] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[idx:])
}
