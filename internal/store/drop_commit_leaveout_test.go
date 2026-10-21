package store

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

func lostHashes(l []LostCommit) []string {
	var out []string
	for _, c := range l {
		out = append(out, c.Commit)
	}
	return out
}

// #533 review 3 SF1: the foreign_commit refusal carries what the confirmed
// fallback (leave the whole merge out) would lose: the merge and every
// commit its side brought, by author and subject. The fallback, confirmed
// with exactly that list, leaves them out and keeps the rest of the branch
// (a later own commit and a merge that does not carry the dropped commit).
//
// SABOTAGE: replay a carrying merge as a merge in the fallback -> bad.md is
// still on the branch -> red.
func TestDrop_TheForeignRefusalListsWhatLeavingTheMergeOutLosesAndTheConfirmedFallbackDoesIt(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	side := h.sideMerge(t, "kb/notes/bad.md", "kb/notes/other.md") // authored "side"
	merge := h.ref(hostBranch)
	_, err := h.svc.rh.CommitFilesForTest(hostBranch, map[string]string{"kb/notes/mine.md": testFactBody("mine", 0.9, nil)}, "mine")
	require.NoError(t, err)
	before := h.ref(hostBranch)

	_, _, err = h.svc.DropUnpublishedCommit(ctx, hostBranch, side[0], true)
	var fe *DropForeignError
	require.True(t, errors.As(err, &fe), "want a foreign refusal, got %v", err)
	require.ElementsMatch(t, []string{merge.String(), side[0].String(), side[1].String()}, lostHashes(fe.LeaveOut))
	for _, l := range fe.LeaveOut {
		if l.Commit == side[0].String() {
			require.Equal(t, "side", l.Agent)
			require.Equal(t, "side kb/notes/bad.md", l.Subject)
			require.NotEmpty(t, l.Author)
		}
	}
	require.Equal(t, before, h.ref(hostBranch), "the refusal changes nothing")

	// A stale confirmation changes nothing either.
	_, _, err = h.svc.DropUnpublishedCommitLeavingOutMerges(ctx, hostBranch, side[0], []string{merge.String()})
	var ch *LeaveOutChangedError
	require.True(t, errors.As(err, &ch), "want leave-out-changed, got %v", err)
	require.Equal(t, before, h.ref(hostBranch))

	replayed, left, err := h.svc.DropUnpublishedCommitLeavingOutMerges(ctx, hostBranch, side[0], lostHashes(fe.LeaveOut))
	require.NoError(t, err)
	require.Equal(t, 1, replayed, "mine.md's commit")
	require.ElementsMatch(t, lostHashes(fe.LeaveOut), lostHashes(left))
	files := h.branchFiles(hostBranch)
	require.NotContains(t, files, "kb/notes/bad.md")
	require.NotContains(t, files, "kb/notes/other.md", "the whole merge is left out")
	require.Contains(t, files, "kb/notes/mine.md")
}

// The merge_commit refusal carries the same fallback: the merge named as
// the target is left out whole once confirmed.
//
// SABOTAGE: drop the LeaveOut list from the merge refusal -> red.
func TestDrop_TheMergeRefusalOffersLeavingTheMergeOut(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	side := h.sideMerge(t, "kb/notes/bad.md")
	m := h.ref(hostBranch)
	_, _, err := h.svc.DropUnpublishedCommit(ctx, hostBranch, m, true)
	var me *DropMergeError
	require.True(t, errors.As(err, &me), "want a merge refusal, got %v", err)
	require.ErrorIs(t, err, ErrDropMerge)
	require.ElementsMatch(t, []string{m.String(), side[0].String()}, lostHashes(me.LeaveOut))
	require.Equal(t, m, h.ref(hostBranch))

	_, left, err := h.svc.DropUnpublishedCommitLeavingOutMerges(ctx, hostBranch, m, lostHashes(me.LeaveOut))
	require.NoError(t, err)
	require.Len(t, left, 2)
	require.NotContains(t, h.branchFiles(hostBranch), "kb/notes/bad.md")
}

// #533 review 3 NIT1: an unfolded side's commits are replayed parents
// first. The side writes bad.md, then edits other.md twice; dropping bad.md
// keeps other.md at its LAST version.
//
// SABOTAGE (the reviewer's SB4): reverse each unfolded side before replay
// -> other.md ends at its first version -> red.
func TestDrop_AnUnfoldedSideIsReplayedParentsFirst(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	require.NoError(t, h.svc.Branches().CreateBranch(ctx, "agent/side", hostBranch))
	var side []plumbing.Hash
	for _, w := range []struct{ p, title string }{
		{"kb/notes/bad.md", "bad"},
		{"kb/notes/other.md", "other first"},
		{"kb/notes/other.md", "other second"},
	} {
		c, err := h.svc.rh.CommitFilesForTest("agent/side", map[string]string{w.p: testFactBody(w.title, 0.9, nil)}, w.title)
		require.NoError(t, err)
		side = append(side, plumbing.NewHash(c))
	}
	_, err := h.svc.MergePushed(ctx, "agent/side", hostBranch, side[2], "")
	require.NoError(t, err)

	replayed, merges, err := h.svc.DropUnpublishedCommit(ctx, hostBranch, side[0], false)
	require.NoError(t, err)
	require.Equal(t, 1, merges)
	require.Equal(t, 2, replayed)
	files := h.branchFiles(hostBranch)
	require.NotContains(t, files, "kb/notes/bad.md")
	require.Equal(t, testFactBody("other second", 0.9, nil), files["kb/notes/other.md"])
}
