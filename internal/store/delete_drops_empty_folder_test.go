package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// #375: knomit's deletes write trees the way git does — a folder whose last
// entry is removed is dropped from its parent, never kept as an empty tree.
// Every delete path goes through deleteFromTree: DeleteFact, BatchWriteFacts,
// and the merge's src-delete.

// treeOf returns the root tree hash at branch's tip.
func treeOf(t *testing.T, svc *Service, branch string) string {
	t.Helper()
	head, err := svc.Branches().HeadCommit(context.Background(), branch)
	require.NoError(t, err)
	c, err := svc.rh.repo.CommitObject(plumbing.NewHash(head))
	require.NoError(t, err)
	return c.TreeHash.String()
}

func TestDeleteFact_LastFactDropsFolder(t *testing.T) {
	svc := newChangesService(t)
	writeF(t, svc, "main", "kb/other/seed.md")
	writeF(t, svc, "main", "kb/x/y/only.md")

	deleteF(t, svc, "main", "kb/x/y/only.md")

	require.False(t, folderPresent(t, svc, "main", "kb/x/y"), "the emptied folder is dropped")
	require.False(t, folderPresent(t, svc, "main", "kb/x"), "the drop cascades: kb/x held only y")
	require.True(t, folderPresent(t, svc, "main", "kb"), "kb still holds other/")
	require.True(t, folderPresent(t, svc, "main", "kb/other"), "a sibling folder is untouched")
	_, err := svc.Facts().ReadFact(context.Background(), "main", "kb/other/seed.md", nil)
	require.NoError(t, err)

	// The dropped folders' empty trees are never stored, so nothing is left
	// unreachable for Verify to report as an orphan.
	rep, err := svc.Verify(context.Background(), VerifyOpts{})
	require.NoError(t, err)
	require.True(t, rep.IsStrictlyClean(), "a cascading drop must leave no orphan objects: %v", rep.Issues)
}

func TestDeleteFact_FolderWithOtherEntriesStays(t *testing.T) {
	svc := newChangesService(t)
	writeF(t, svc, "main", "kb/x/y/a.md")
	writeF(t, svc, "main", "kb/x/y/b.md")

	deleteF(t, svc, "main", "kb/x/y/a.md")

	require.True(t, folderPresent(t, svc, "main", "kb/x/y"), "a folder that still holds b.md stays")
}

func TestBatchWriteFacts_MoveDropsEmptiedFolder(t *testing.T) {
	svc := newChangesService(t)
	writeF(t, svc, "main", "kb/tasks/a/old.md")

	_, _, err := svc.Facts().BatchWriteFacts(context.Background(), "main",
		map[string]string{"kb/tasks/b/new.md": testFactBody("kb/tasks/b/new.md", 0.8, nil)},
		[]string{"kb/tasks/a/old.md"}, "move", "learn")
	require.NoError(t, err)

	require.False(t, folderPresent(t, svc, "main", "kb/tasks/a"), "the lane the task left is dropped")
	require.True(t, folderPresent(t, svc, "main", "kb/tasks/b"))
}

// Deleting the same last fact twice. The first delete drops the folder. The
// second differs by entry point, and neither changes with #375 except that the
// folder is now gone:
//   - DeleteFact checks existence under the branch lock and refuses an absent
//     path with an error, as it always has.
//   - BatchWriteFacts treats the absent path as a no-op (option b: a missing
//     folder behaves like a missing leaf did). It still commits, with the tree
//     unchanged — the same outcome a missing-leaf delete has always had.
func TestDelete_SameLastFactTwice(t *testing.T) {
	ctx := context.Background()
	svc := newChangesService(t)
	writeF(t, svc, "main", "kb/other/seed.md")
	writeF(t, svc, "main", "kb/x/y/only.md")

	deleteF(t, svc, "main", "kb/x/y/only.md")
	require.False(t, folderPresent(t, svc, "main", "kb/x"), "first delete drops the folder")
	tree := treeOf(t, svc, "main")
	head, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)

	_, err = svc.Facts().DeleteFact(ctx, "main", "kb/x/y/only.md", "again")
	require.ErrorIs(t, err, ErrPathNotFound, "DeleteFact keeps its own existence check, wrapped as ErrPathNotFound (F25 follow-up) so a REST DELETE of a missing path can answer 404 instead of 500")
	after, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, head, after, "a refused DeleteFact writes nothing")

	_, _, err = svc.Facts().BatchWriteFacts(ctx, "main", nil, []string{"kb/x/y/only.md"}, "again", "learn")
	require.NoError(t, err, "a batch delete of a path in a missing folder is a no-op, not an error")
	require.Equal(t, tree, treeOf(t, svc, "main"), "the no-op delete leaves the tree unchanged")
}

func TestMergeBranch_SrcDeleteDropsEmptiedFolder(t *testing.T) {
	ctx := context.Background()
	svc := newMergeTestStore(t)
	writeMergeFact(t, svc, "main", "kb/other/seed.md", "seed", "body")
	writeMergeFact(t, svc, "main", "kb/lane/only.md", "only", "body")
	require.NoError(t, svc.Branches().CreateBranch(ctx, "feature", "main"))

	// feature deletes the lane's last file; main moves on elsewhere, so the
	// merge is a real three-way merge, not a fast-forward.
	_, err := svc.Facts().DeleteFact(ctx, "feature", "kb/lane/only.md", "drop")
	require.NoError(t, err)
	writeMergeFact(t, svc, "main", "kb/other/more.md", "more", "body")

	require.NoError(t, svc.Branches().MergeBranch(ctx, "feature", "main", StrategyLocalWins))

	require.False(t, folderPresent(t, svc, "main", "kb/lane"), "the merge drops the folder src emptied")
	require.True(t, folderPresent(t, svc, "main", "kb/other"))
	verifyMergeClean(t, svc, "main")
}
