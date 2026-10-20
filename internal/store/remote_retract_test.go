package store

import (
	"context"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// RetractPushedBranch deletes the branch this instance pushed, then removes
// the origin remote, so that the next Push sends nothing. The tracking ref
// goes too.
// Sabotage: drop the DeleteRemote step -> the next Push recreates the branch
// on the origin -> red.
func TestRetractPushedBranch_DeletesWhatThisInstancePushed(t *testing.T) {
	ctx := context.Background()
	svc, bare := newSnapshotPushFixture(t)
	_, err := svc.Remote().Push(ctx, snapBranch, nil)
	require.NoError(t, err)
	require.Equal(t, localTip(t, svc), originTip(t, bare), "pushed")

	require.NoError(t, svc.Remote().RetractPushedBranch(ctx, snapBranch, nil))
	require.True(t, originTip(t, bare).IsZero(), "the origin's branch is deleted")
	_, err = svc.rh.Storer().Reference(plumbing.NewRemoteReferenceName("origin", snapBranch))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "the tracking ref is gone")

	writeTestFact(t, svc, "kb/notes/later.md", "later", "later")
	res, err := svc.Remote().Push(ctx, snapBranch, nil)
	require.NoError(t, err)
	require.False(t, res.Pushed, "no origin remote: the next push sends nothing")
	require.True(t, originTip(t, bare).IsZero(), "nothing recreated the branch")

	require.NoError(t, svc.Remote().RetractPushedBranch(ctx, snapBranch, nil), "a re-run is a no-op")
}

// The lease: a tip on the origin that is not the hash this instance pushed is
// never deleted, and the origin remote stays for a retry.
// Sabotage: drop the at != pushed refusal and the RequireRemoteRefs lease ->
// the foreign tip is deleted -> red.
func TestRetractPushedBranch_LeavesAForeignTipAlone(t *testing.T) {
	ctx := context.Background()
	svc, bare := newSnapshotPushFixture(t)
	_, err := svc.Remote().Push(ctx, snapBranch, nil)
	require.NoError(t, err)
	pushed := originTip(t, bare)

	// Someone else moves the origin's branch: to the pushed commit's parent,
	// an object the origin already has.
	repo, err := gogit.PlainOpen(bare)
	require.NoError(t, err)
	c, err := repo.CommitObject(pushed)
	require.NoError(t, err)
	require.NotEmpty(t, c.ParentHashes)
	foreign := c.ParentHashes[0]
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(snapBranch), foreign)))

	err = svc.Remote().RetractPushedBranch(ctx, snapBranch, nil)
	require.ErrorContains(t, err, "left alone")
	require.ErrorContains(t, err, foreign.String())
	require.ErrorContains(t, err, pushed.String())
	require.Equal(t, foreign, originTip(t, bare), "the foreign tip is untouched")
	_, err = svc.rh.Repository().Remote("origin")
	require.NoError(t, err, "the origin remote is kept for a retry")
}

// No tracking ref: this instance never pushed the branch the origin has, so
// nothing is deleted. A branch absent on the origin is not an error.
func TestRetractPushedBranch_NeverPushedIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	fresh, freshBare := newSnapshotPushFixture(t)
	require.NoError(t, fresh.Remote().RetractPushedBranch(ctx, snapBranch, nil), "absent on the origin, never pushed: nothing to do")
	require.True(t, originTip(t, freshBare).IsZero())

	svc, bare := newSnapshotPushFixture(t)
	_, err := svc.Remote().Push(ctx, snapBranch, nil)
	require.NoError(t, err)
	require.NoError(t, svc.rh.Storer().RemoveReference(plumbing.NewRemoteReferenceName("origin", snapBranch)))
	err = svc.Remote().RetractPushedBranch(ctx, snapBranch, nil)
	require.ErrorContains(t, err, "never pushed it")
	require.False(t, originTip(t, bare).IsZero(), "the branch is still on the origin")
}
