package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// DropUnpublishedCommit replays the other unpublished commits IN ORDER: x.md
// is created, edited, edited again around the dropped commit, and comes out
// at its LAST version (#533 review S2).
//
// SABOTAGE: replay the line in reverse (oldest last) -> x.md comes out at
// its first version -> red.
func TestDrop_ReplaysTheOtherCommitsInOrder(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	write := func(p, c string) {
		_, err := h.svc.Facts().WriteFact(ctx, hostBranch, p, c, "w "+p, "learn")
		require.NoError(t, err)
	}
	write("kb/notes/x.md", testFactBody("x v1", 0.9, nil))
	bad, err := h.svc.rh.CommitFilesForTest(hostBranch, map[string]string{"kb/notes/bad.md": testFactBody("bad", 0.9, nil)}, "bad")
	require.NoError(t, err)
	write("kb/notes/x.md", testFactBody("x v2", 0.8, nil))
	write("kb/notes/x.md", testFactBody("x v3", 0.7, nil))

	replayed, merges, err := h.svc.Remote().DropUnpublishedCommit(ctx, hostBranch, plumbing.NewHash(bad), false)
	require.NoError(t, err)
	require.Equal(t, 3, replayed)
	require.Zero(t, merges)
	files := h.branchFiles(hostBranch)
	require.Contains(t, files["kb/notes/x.md"], "x v3", "the last edit wins")
	require.NotContains(t, files, "kb/notes/bad.md")
}

// A later commit's edit of the file the dropped commit wrote is kept whole
// (#533 review S3): what the drop removes is the dropped commit's OWN
// change; the copy says so.
//
// SABOTAGE: replay with the rebuilt line winning conflicts instead of the
// replayed commit -> the later edit is lost and x.md is gone -> red.
func TestDrop_KeepsALaterEditOfTheDroppedCommitsFile(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	bad, err := h.svc.rh.CommitFilesForTest(hostBranch, map[string]string{"kb/notes/x.md": testFactBody("x bad", 0.9, nil)}, "bad")
	require.NoError(t, err)
	_, err = h.svc.Facts().WriteFact(ctx, hostBranch, "kb/notes/x.md", testFactBody("x fixed", 0.5, nil), "fix", "updated")
	require.NoError(t, err)
	_, _, err = h.svc.Remote().DropUnpublishedCommit(ctx, hostBranch, plumbing.NewHash(bad), false)
	require.NoError(t, err)
	require.Contains(t, h.branchFiles(hostBranch)["kb/notes/x.md"], "x fixed")
}

// Merge commits on the line are replayed as merges of the same parents
// (#533 review S1): a committed experiment, whose exp branch is deleted on
// commit, keeps its fact. A merge that brought the DROPPED commit in is left
// out, so the dropped commit does not come back through it.
//
// SABOTAGE: leave every merge out (the pre-fix behaviour) -> the
// experiment's fact is lost -> red; replay every merge, the carrying one
// included -> the dropped fact comes back -> red.
func TestDrop_ReplaysMergesAndLeavesOutTheOneThatBroughtTheDroppedCommit(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))

	ex, err := h.svc.Merge().OpenExperiment(ctx, "e1", "d", hostBranch)
	require.NoError(t, err)
	_, err = h.svc.Facts().WriteFact(ctx, ex.Branch(), "kb/notes/exp.md", testFactBody("exp", 0.9, nil), "exp", "created")
	require.NoError(t, err)
	_, err = h.svc.Facts().WriteFact(ctx, hostBranch, "kb/notes/a.md", testFactBody("a", 0.9, nil), "a", "created")
	require.NoError(t, err)
	_, err = h.svc.Merge().CommitExperiment(ctx, "e1", nil)
	require.NoError(t, err)
	require.Contains(t, h.branchFiles(hostBranch), "kb/notes/exp.md", "fixture: the experiment is merged")

	// A side branch whose commit is the one to drop, merged in.
	require.NoError(t, h.svc.Branches().CreateBranch(ctx, "agent/side", hostBranch))
	bad, err := h.svc.rh.CommitFilesForTest("agent/side", map[string]string{"kb/notes/bad.md": testFactBody("bad", 0.9, nil)}, "bad")
	require.NoError(t, err)
	_, err = h.svc.MergePushed(ctx, "agent/side", hostBranch, plumbing.NewHash(bad), "")
	require.NoError(t, err)
	_, err = h.svc.Facts().WriteFact(ctx, hostBranch, "kb/notes/c.md", testFactBody("c", 0.9, nil), "c", "created")
	require.NoError(t, err)

	_, merges, err := h.svc.Remote().DropUnpublishedCommit(ctx, hostBranch, plumbing.NewHash(bad), false)
	require.NoError(t, err)
	require.Equal(t, 1, merges, "the merge that brought the dropped commit is left out")
	files := h.branchFiles(hostBranch)
	for _, p := range []string{"kb/notes/a.md", "kb/notes/c.md", "kb/notes/exp.md"} {
		require.Contains(t, files, p)
	}
	require.NotContains(t, files, "kb/notes/bad.md")
}
