package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// All tests use the same setup: a fresh store with a "main" branch holding
// an initial fact, and a "feature" branch forked from main at a known commit.
// Each test then diverges history on both branches and calls MergeBranch.

func newMergeTestStore(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc
}

// writeMergeFact writes a minimal parseable fact to the branch and returns
// the commit hash. Uses the same shape as verify_test.go so it sails through
// fact.ParseFact and the Verify deep checks.
func writeMergeFact(t *testing.T, svc *Service, branch, path, title, body string) string {
	t.Helper()
	content := "---\ntype: observation\n---\n# " + title + "\n\n" + body + "\n"
	res, err := svc.Facts().WriteFact(context.Background(), branch, path, content, "test", "test")
	require.NoError(t, err)
	return res.CommitHash
}

// verifyMergeClean syncs the search index for dst (MergeBranch only touches
// git state; branch_facts/search index must be reconciled separately, just
// like production Sync does via the onCommit observer) and runs a deep Verify.
func verifyMergeClean(t *testing.T, svc *Service, dst string) {
	t.Helper()
	require.NoError(t, svc.IndexManager().Sync(context.Background(), dst))
	report, err := svc.Verify(context.Background(), VerifyOpts{Deep: true})
	require.NoError(t, err)
	require.True(t, report.IsClean(), "integrity issues: %v", report.Issues)
}

// TestMergeBranch_SameHashNoOp: merging a branch with itself (or two pointers
// at the same commit) is a clean no-op.
func TestMergeBranch_SameHashNoOp(t *testing.T) {
	t.Log("Scenario: src and dst point at the same commit — MergeBranch is a no-op, integrity stays clean")
	svc := newMergeTestStore(t)

	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_AlreadyMergedNoOp: src is an ancestor of dst — nothing to do.
func TestMergeBranch_AlreadyMergedNoOp(t *testing.T) {
	t.Log("Scenario: src is strict ancestor of dst (dst has moved ahead) — MergeBranch is a no-op")
	svc := newMergeTestStore(t)

	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))
	// Advance main ahead of feature.
	writeMergeFact(t, svc, "main", "kb/a.md", "a", "body")

	beforeHash, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	afterHash, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, beforeHash, afterHash, "main HEAD must not advance when src is ancestor")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_FastForward: dst is an ancestor of src — advance dst ref.
func TestMergeBranch_FastForward(t *testing.T) {
	t.Log("Scenario: dst is ancestor of src (src has moved ahead) — fast-forward advance")
	svc := newMergeTestStore(t)

	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))
	srcHash := writeMergeFact(t, svc, "feature", "kb/a.md", "a", "body")

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	mainHead, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)
	require.Equal(t, srcHash, mainHead, "main should fast-forward to feature's HEAD")

	// Verify the fact is now reachable from main.
	res, err := svc.Facts().ReadFact(context.Background(), "main", "kb/a.md", nil)
	require.NoError(t, err)
	require.Contains(t, res.Content, "# a")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_ThreeWayDisjointChanges: both branches added different files.
// Both strategies should produce the same result (no conflicts).
func TestMergeBranch_ThreeWayDisjointChanges(t *testing.T) {
	t.Log("Scenario: main adds A, feature adds B — three-way merge yields both under both strategies")
	for _, strat := range []ConflictStrategy{StrategyLocalWins, StrategyRemoteWins} {
		t.Run(string(strat), func(t *testing.T) {
			svc := newMergeTestStore(t)
			require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

			writeMergeFact(t, svc, "main", "kb/a.md", "a", "a body")
			writeMergeFact(t, svc, "feature", "kb/b.md", "b", "b body")

			require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", strat))

			resA, err := svc.Facts().ReadFact(context.Background(), "main", "kb/a.md", nil)
			require.NoError(t, err)
			require.Contains(t, resA.Content, "# a")

			resB, err := svc.Facts().ReadFact(context.Background(), "main", "kb/b.md", nil)
			require.NoError(t, err)
			require.Contains(t, resB.Content, "# b")

			verifyMergeClean(t, svc, "main")
		})
	}
}

// TestMergeBranch_ThreeWayConflictLocalWins: both branches modified the same
// file. StrategyLocalWins keeps dst's (main's) version.
func TestMergeBranch_ThreeWayConflictLocalWins(t *testing.T) {
	t.Log("Scenario: main and feature both modify kb/x.md; LocalWins keeps main's version")
	svc := newMergeTestStore(t)
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "original")
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	writeMergeFact(t, svc, "main", "kb/x.md", "x", "main version")
	writeMergeFact(t, svc, "feature", "kb/x.md", "x", "feature version")

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	res, err := svc.Facts().ReadFact(context.Background(), "main", "kb/x.md", nil)
	require.NoError(t, err)
	require.Contains(t, res.Content, "main version")
	require.NotContains(t, res.Content, "feature version")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_ThreeWayConflictRemoteWins: both branches modified the same
// file. StrategyRemoteWins keeps src's (feature's) version.
func TestMergeBranch_ThreeWayConflictRemoteWins(t *testing.T) {
	t.Log("Scenario: main and feature both modify kb/x.md; RemoteWins takes feature's version")
	svc := newMergeTestStore(t)
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "original")
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	writeMergeFact(t, svc, "main", "kb/x.md", "x", "main version")
	writeMergeFact(t, svc, "feature", "kb/x.md", "x", "feature version")

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyRemoteWins))

	res, err := svc.Facts().ReadFact(context.Background(), "main", "kb/x.md", nil)
	require.NoError(t, err)
	require.Contains(t, res.Content, "feature version")
	require.NotContains(t, res.Content, "main version")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_DeletedOnSrcNonConflict: src deleted a file, dst left it
// alone. Non-conflicting — the delete applies in both strategies.
func TestMergeBranch_DeletedOnSrcNonConflict(t *testing.T) {
	t.Log("Scenario: feature deletes kb/x.md, main did not touch it — delete applies in both strategies")
	svc := newMergeTestStore(t)
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "body")
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	_, err := svc.Facts().DeleteFact(context.Background(), "feature", "kb/x.md", "drop x")
	require.NoError(t, err)

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	_, err = svc.Facts().ReadFact(context.Background(), "main", "kb/x.md", nil)
	require.Error(t, err, "kb/x.md should be gone after merging the delete")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_DeletedOnSrcConflictLocalWins: src deleted, dst modified.
// LocalWins keeps dst's modification.
func TestMergeBranch_DeletedOnSrcConflictLocalWins(t *testing.T) {
	t.Log("Scenario: feature deletes kb/x.md, main modified it — LocalWins keeps main's modified version")
	svc := newMergeTestStore(t)
	writeMergeFact(t, svc, "main", "kb/x.md", "x", "original")
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	writeMergeFact(t, svc, "main", "kb/x.md", "x", "main modification")
	_, err := svc.Facts().DeleteFact(context.Background(), "feature", "kb/x.md", "drop x")
	require.NoError(t, err)

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	res, err := svc.Facts().ReadFact(context.Background(), "main", "kb/x.md", nil)
	require.NoError(t, err, "kb/x.md should still exist because main's modification wins")
	require.Contains(t, res.Content, "main modification")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_MergeCommitHasTwoParents: after a three-way merge, the
// resulting commit has exactly two parents in the expected order.
func TestMergeBranch_MergeCommitHasTwoParents(t *testing.T) {
	t.Log("Scenario: three-way merge commit has [dst, src] parents in that order")
	svc := newMergeTestStore(t)
	require.NoError(t, svc.Branches().CreateBranch(context.Background(), "feature", "main"))

	dstHashBefore := writeMergeFact(t, svc, "main", "kb/a.md", "a", "main fact")
	srcHash := writeMergeFact(t, svc, "feature", "kb/b.md", "b", "feature fact")

	require.NoError(t, svc.Branches().MergeBranch(context.Background(), "feature", "main", StrategyLocalWins))

	mergeHashStr, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)

	mergeHash := plumbing.NewHash(mergeHashStr)
	mc, err := svc.rh.repo.CommitObject(mergeHash)
	require.NoError(t, err)
	require.Len(t, mc.ParentHashes, 2, "merge commit must have two parents")
	require.Equal(t, dstHashBefore, mc.ParentHashes[0].String(), "first parent must be dst HEAD before merge")
	require.Equal(t, srcHash, mc.ParentHashes[1].String(), "second parent must be src HEAD")

	verifyMergeClean(t, svc, "main")
}

// TestMergeBranch_MissingSrcReturnsError: merging a non-existent src fails.
func TestMergeBranch_MissingSrcReturnsError(t *testing.T) {
	t.Log("Scenario: MergeBranch(nonexistent, main) returns an error, does not panic")
	svc := newMergeTestStore(t)
	err := svc.Branches().MergeBranch(context.Background(), "does-not-exist", "main", StrategyLocalWins)
	require.Error(t, err)
}

func TestMergeIntoBranch_NoopWhenSrcAncestorOfDst(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// main and agent both at init commit; src=main, dst=agent → no-op.
	res, err := svc.rh.mergeIntoBranch(context.Background(), "main", "agent/test", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, "noop", res.Mode)
	require.False(t, res.Merged)
	require.False(t, res.FastForward)
}

func TestMergeIntoBranch_FastForwardWhenDstAncestorOfSrc(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	mainHash := writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")

	res, err := svc.rh.mergeIntoBranch(context.Background(), "main", "agent/test", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, "ff", res.Mode)
	require.True(t, res.FastForward)
	require.Equal(t, mainHash, res.NewTip)
	require.Equal(t, plumbing.NewHash(mainHash), mustHeadHash(t, svc, "agent/test"))
}

func TestMergeIntoBranch_DivergentCreatesOneMergeCommit(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// Divergent: agent writes one file, main writes another.
	writeMergeFact(t, svc, "agent/test", "kb/a.md", "A", "v1")
	writeMergeFact(t, svc, "main", "kb/m.md", "M", "v1")

	preAgentTip := mustHeadHash(t, svc, "agent/test")
	preMainTip := mustHeadHash(t, svc, "main")

	res, err := svc.rh.mergeIntoBranch(context.Background(), "main", "agent/test", StrategyLocalWins)
	require.NoError(t, err)
	require.Equal(t, "merge", res.Mode)
	require.True(t, res.Merged)
	require.False(t, res.FastForward)
	require.NotEmpty(t, res.NewTip)

	newTip, err := svc.rh.repo.CommitObject(plumbing.NewHash(res.NewTip))
	require.NoError(t, err)
	require.Equal(t, 2, newTip.NumParents(), "merge commit has two parents")
	require.Equal(t, preAgentTip, newTip.ParentHashes[0], "first parent is previous agent tip (ours)")
	require.Equal(t, preMainTip, newTip.ParentHashes[1], "second parent is local main (theirs)")
}
