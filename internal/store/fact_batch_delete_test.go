package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// newBatchTestService opens a fresh initialised store for the batch tests.
func newBatchTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	return svc
}

// TestBatchWriteFacts_WriteAndDeleteShareOneCommit is the regression test for
// the learn-subsume bug (P0.3): the retraction of a subsumed hypothesis used to
// be a separate DeleteFact commit, so a failure between the two could leave the
// new observation referencing a hypothesis it claimed to subsume. Writes and
// deletions passed to one BatchWriteFacts call must land in a SINGLE commit.
func TestBatchWriteFacts_WriteAndDeleteShareOneCommit(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	old, err := facts.WriteFact(ctx, "main", "kb/old.md", testFactBody("old", 0.5, nil), "seed", "")
	require.NoError(t, err)

	commit, blobs, err := facts.BatchWriteFacts(ctx, "main",
		map[string]string{"kb/new.md": testFactBody("new", 0.9, nil)},
		[]string{"kb/old.md"},
		"subsume", "learn")
	require.NoError(t, err)
	require.NotEmpty(t, commit)
	require.Contains(t, blobs, "kb/new.md")

	// The write is visible and the deletion applied — both at the same commit.
	_, err = facts.ReadFact(ctx, "main", "kb/new.md", nil)
	require.NoError(t, err, "batched write must be readable")

	_, err = facts.ReadFact(ctx, "main", "kb/old.md", nil)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"batched delete must remove the fact, got %v", err)

	// One commit, not two: the batch commit's parent is the seed commit.
	added, modified, deleted, err := facts.DiffFiles(ctx, "main", old.CommitHash)
	require.NoError(t, err)
	require.Equal(t, []string{"kb/new.md"}, added)
	require.Empty(t, modified)
	require.Equal(t, []string{"kb/old.md"}, deleted)
}

// TestBatchWriteFacts_DeleteOnlyBatch covers the delete-only case: with no
// writes there is nothing to advance the running tree, so the commit must be
// seeded from the parent tree rather than committing an empty one.
func TestBatchWriteFacts_DeleteOnlyBatch(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	_, err := facts.WriteFact(ctx, "main", "kb/keep.md", testFactBody("keep", 0.5, nil), "seed", "")
	require.NoError(t, err)
	_, err = facts.WriteFact(ctx, "main", "kb/drop.md", testFactBody("drop", 0.5, nil), "seed", "")
	require.NoError(t, err)

	commit, _, err := facts.BatchWriteFacts(ctx, "main", nil, []string{"kb/drop.md"}, "retract", "learn")
	require.NoError(t, err)
	require.NotEmpty(t, commit)

	_, err = facts.ReadFact(ctx, "main", "kb/keep.md", nil)
	require.NoError(t, err, "untouched fact must survive a delete-only batch")
	_, err = facts.ReadFact(ctx, "main", "kb/drop.md", nil)
	require.True(t, errors.Is(err, ErrPathNotFound), "expected deletion, got %v", err)
}

// TestBatchWriteFacts_EmptyBatchIsNoOp: an all-empty call must not mint a
// commit. Callers (learn with nothing to retract) rely on this.
func TestBatchWriteFacts_EmptyBatchIsNoOp(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()

	commit, blobs, err := svc.Facts().BatchWriteFacts(ctx, "main", nil, nil, "nothing", "learn")
	require.NoError(t, err)
	require.Empty(t, commit)
	require.Empty(t, blobs)
}

// TestBatchWriteFacts_DeleteInMissingFolderIsNoOp: a deletion naming a path
// whose folder does not exist is a no-op, exactly like a missing leaf in an
// existing folder — nothing is there, so there is nothing to delete (#375,
// option b). The batch's write still lands. Callers that need the path to
// exist say so with BatchWriteFactsMustExist.
func TestBatchWriteFacts_DeleteInMissingFolderIsNoOp(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	_, err := facts.WriteFact(ctx, "main", "kb/a.md", testFactBody("a", 0.5, nil), "seed", "")
	require.NoError(t, err)

	_, _, err = facts.BatchWriteFacts(ctx, "main",
		map[string]string{"kb/b.md": testFactBody("b", 0.9, nil)},
		[]string{"kb/nested/missing.md"},
		"write plus absent delete", "learn")
	require.NoError(t, err)

	_, err = facts.ReadFact(ctx, "main", "kb/b.md", nil)
	require.NoError(t, err, "the write must land; the absent delete removes nothing")
	_, err = facts.ReadFact(ctx, "main", "kb/a.md", nil)
	require.NoError(t, err)
}

// TestBatchWriteFacts_DeleteFailureCommitsNothing: a deletion that FAILS must
// fail the whole batch rather than silently committing the writes alone — the
// caller asked for both or neither. A missing folder is no longer a failure
// (see above), so the failure here is a real one: a subtree object the tree
// names but the store does not hold.
func TestBatchWriteFacts_DeleteFailureCommitsNothing(t *testing.T) {
	svc := newBatchTestService(t)
	ctx := context.Background()
	facts := svc.Facts()

	_, err := facts.WriteFact(ctx, "main", "kb/a.md", testFactBody("a", 0.5, nil), "seed", "")
	require.NoError(t, err)
	before := commitBrokenSubtree(t, svc, "main", "kb", "broken")

	_, _, err = facts.BatchWriteFacts(ctx, "main",
		map[string]string{"kb/b.md": testFactBody("b", 0.9, nil)},
		[]string{"kb/broken/x.md"},
		"bad", "learn")
	require.Error(t, err)

	head, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, before, head, "a failed batch must not move the branch")
	_, err = facts.ReadFact(ctx, "main", "kb/b.md", nil)
	require.True(t, errors.Is(err, ErrPathNotFound),
		"a failed batch must not leave the write committed, got %v", err)
}

// commitBrokenSubtree commits, on top of branch's tip, a tree whose folder dir
// gains a sub-folder entry name pointing at a tree object the store does not
// hold, and moves the branch there. It returns the new tip.
func commitBrokenSubtree(t *testing.T, svc *Service, branch, dir, name string) string {
	t.Helper()
	rh := svc.rh
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	tip, err := rh.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	root, err := tip.Tree()
	require.NoError(t, err)
	sub, err := root.Tree(dir)
	require.NoError(t, err)

	missing := plumbing.NewHash("1111111111111111111111111111111111111111")
	subHash, err := upsertEntry(rh.gits, sub, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: missing})
	require.NoError(t, err)
	rootHash, err := upsertEntry(rh.gits, root, object.TreeEntry{Name: dir, Mode: filemode.Dir, Hash: subHash})
	require.NoError(t, err)

	c := &object.Commit{Author: tip.Author, Committer: tip.Committer, Message: "broken subtree",
		TreeHash: rootHash, ParentHashes: []plumbing.Hash{tip.Hash}}
	obj := rh.gits.NewEncodedObject()
	require.NoError(t, c.Encode(obj))
	h, err := rh.gits.SetEncodedObject(obj)
	require.NoError(t, err)
	require.NoError(t, rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)))
	return h.String()
}
