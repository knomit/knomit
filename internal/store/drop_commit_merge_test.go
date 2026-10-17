package store

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// sideMerge puts commits on agent/side (one file each, in order) and merges
// the side into the host's branch through MergePushed: the hashes of the side
// commits.
func (h *pushHost) sideMerge(t *testing.T, files ...string) []plumbing.Hash {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.svc.Branches().CreateBranch(ctx, "agent/side", hostBranch))
	var out []plumbing.Hash
	for _, p := range files {
		c, err := h.svc.TestingCommitFiles("agent/side", map[string]string{p: testFactBody(p, 0.9, nil)}, "side "+p)
		require.NoError(t, err)
		out = append(out, plumbing.NewHash(c))
	}
	_, err := h.svc.MergePushed(ctx, "agent/side", hostBranch, out[len(out)-1], "")
	require.NoError(t, err)
	return out
}

// #533 re-review S1, probe D1 made a test: the merge that brought the
// dropped commit in ALSO brought another commit of the same side branch.
// The merge is unfolded: the side's other commit is replayed, so other.md is
// kept, and only the dropped commit's own file is gone.
//
// SABOTAGE: leave a carrying merge out whole (the previous behaviour) ->
// other.md is lost -> red.
func TestDrop_D1_TheCarryingMergesOtherCommitsAreKept(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	side := h.sideMerge(t, "kb/notes/bad.md", "kb/notes/other.md")
	replayed, merges, err := h.svc.DropUnpublishedCommit(ctx, hostBranch, side[0], false)
	require.NoError(t, err)
	require.Equal(t, 1, merges, "the carrying merge was unfolded")
	require.Equal(t, 1, replayed, "other.md's commit was replayed")
	files := h.branchFiles(hostBranch)
	require.Contains(t, files, "kb/notes/other.md")
	require.NotContains(t, files, "kb/notes/bad.md")
}

// Probe D5 made a test: a side branch (a committed experiment) forked AFTER
// the dropped commit, so its history holds it, with its own work, merged
// back. Its own commit is replayed; exp.md is kept and bad.md is gone.
//
// SABOTAGE: leave a carrying merge out whole -> exp.md is lost for good (the
// experiment's branch was deleted on commit) -> red.
func TestDrop_D5_ASideForkedAfterTheDroppedCommitKeepsItsWork(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	bad, err := h.svc.TestingCommitFiles(hostBranch, map[string]string{"kb/notes/bad.md": testFactBody("bad", 0.9, nil)}, "bad")
	require.NoError(t, err)
	ex, err := h.svc.Experiments().OpenExperiment(ctx, "e1", "d", hostBranch)
	require.NoError(t, err)
	_, err = h.svc.Facts().WriteFact(ctx, ex.Branch(), "kb/notes/exp.md", testFactBody("exp", 0.9, nil), "exp", "created")
	require.NoError(t, err)
	_, err = h.svc.Facts().WriteFact(ctx, hostBranch, "kb/notes/a.md", testFactBody("a", 0.9, nil), "a", "created")
	require.NoError(t, err)
	_, err = h.svc.Experiments().CommitExperiment(ctx, "e1", nil)
	require.NoError(t, err)
	// ownOnly: the experiment's commit is this instance's own.
	_, merges, err := h.svc.DropUnpublishedCommit(ctx, hostBranch, plumbing.NewHash(bad), true)
	require.NoError(t, err)
	require.Equal(t, 1, merges)
	files := h.branchFiles(hostBranch)
	require.Contains(t, files, "kb/notes/exp.md")
	require.Contains(t, files, "kb/notes/a.md")
	require.NotContains(t, files, "kb/notes/bad.md")
}

// Probe D2 made a test (#533 re-review S2): a MERGE commit cannot be the
// drop target: ErrDropMerge, and nothing changes (it used to be replayed, a
// silent no-op reported as a success).
//
// SABOTAGE: drop the merge check -> the drop "succeeds" and bad.md is still
// there -> red.
func TestDrop_D2_AMergeCannotBeDropped(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	h.sideMerge(t, "kb/notes/bad.md")
	m := h.ref(hostBranch)
	_, _, err := h.svc.DropUnpublishedCommit(ctx, hostBranch, m, false)
	require.ErrorIs(t, err, ErrDropMerge)
	require.Equal(t, m, h.ref(hostBranch), "nothing changed")
}

// With ownOnly (the fleet, whose judge refuses another agent's commit
// re-signed by this instance) a carrying merge whose side holds ANOTHER
// agent's commit refuses the drop instead of re-signing it, and nothing
// changes: nothing but the named commit's own change is ever lost.
//
// SABOTAGE: skip the ownOnly check -> the drop goes through, re-signing
// agent side's commit -> red.
func TestDrop_ARemainingCommitOfAnotherAgentRefusesWithOwnOnly(t *testing.T) {
	ctx := context.Background()
	h := newPushHost(t, VerifyOff, 0)
	h.svc.SetSigner(newTestSigner(t))
	side := h.sideMerge(t, "kb/notes/bad.md", "kb/notes/other.md") // authored "side"
	before := h.ref(hostBranch)
	_, _, err := h.svc.DropUnpublishedCommit(ctx, hostBranch, side[0], true)
	var fe *DropForeignError
	require.True(t, errors.As(err, &fe), "want a foreign refusal, got %v", err)
	require.Equal(t, "side", fe.Agent)
	require.Equal(t, side[1].String(), fe.Commit)
	require.Equal(t, before, h.ref(hostBranch), "nothing changed")
}
