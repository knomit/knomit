package remotesync

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/merge"
)

// dropOutcome is what dropUnpublished did: how many commits it replayed, how
// many merges it unfolded, and what it left out (the confirmed fallback).
type dropOutcome struct {
	Replayed, Unfolded int
	LeftOut            []LostCommit
}

// DropUnpublishedCommit is the operator's way out of a refusal by the judge
// of this instance's own commits (JudgeOwnRange): the refused commit stays
// on the agent branch, so every later move of main is refused too, and
// knomit never rewinds a branch by itself. It rebuilds branch on the
// consensus branch's tip, replaying in order every commit of branch ∖ main
// on the first-parent line (re-signed by this instance, its own content
// winning a conflict) EXCEPT drop, so nothing but drop's own change is lost:
//   - a merge commit is replayed as a merge of the same other parents, so
//     what it brought in stays (a member's branch the merger had merged, a
//     committed experiment);
//   - a merge whose other parents reach drop (it brought drop in, or its
//     side forked after drop) is unfolded: its side's own non-merge commits
//     are replayed one by one, parents first, drop excluded (counted in
//     mergesDropped). A conflict resolution the side's own merges made is
//     not kept. With ownOnly (the fleet, whose judge refuses a commit
//     re-signed for another agent) a side commit authored by another agent
//     refuses the drop (*DropForeignError) instead of re-signing it;
//   - where a later commit edited a file drop wrote, the later version is
//     kept, whole (#533 review S3).
//
// drop must be a non-merge commit of branch that main does not hold
// (ErrNotUnpublished, *DropMergeError). Both refusals that leave the
// operator stuck (*DropMergeError, *DropForeignError) carry what
// DropUnpublishedCommitLeavingOutMerges would lose instead. Returns how many
// commits were replayed and how many merges were unfolded.
func (ri *Index) DropUnpublishedCommit(ctx context.Context, branch string, drop plumbing.Hash, ownOnly bool) (replayed, mergesDropped int, err error) {
	out, err := ri.dropUnpublished(ctx, branch, drop, ownOnly, false, nil)
	return out.Replayed, out.Unfolded, err
}

// DropUnpublishedCommitLeavingOutMerges is the CONFIRMED fallback (#533
// review 3 SF1) for when DropUnpublishedCommit refuses because nothing but
// the named commit can be lost and that cannot be kept (a merge as the
// target; another agent's commit an unfold would re-sign, its author gone
// for good): it rebuilds branch on the consensus branch's tip as
// DropUnpublishedCommit does, except that drop, when it is on the
// first-parent line, and every merge on that line whose other parents reach
// drop are left out WHOLE, with every commit their sides brought (no
// unfold, so nothing is re-signed for another agent). Every other commit and
// merge is replayed as DropUnpublishedCommit replays it. Returns how many
// commits were replayed and what was left out (the merges and their sides'
// commits, as the refusal listed them). The caller asks the operator first:
// the left-out work is gone from this branch. confirmed is the list of
// commit hashes the operator was shown and confirmed; when what would be
// left out now differs (the branch moved since), nothing changes and the
// answer is a *LeaveOutChangedError carrying the new list to confirm.
func (ri *Index) DropUnpublishedCommitLeavingOutMerges(ctx context.Context, branch string, drop plumbing.Hash, confirmed []string) (replayed int, leftOut []LostCommit, err error) {
	out, err := ri.dropUnpublished(ctx, branch, drop, false, true, confirmed)
	return out.Replayed, out.LeftOut, err
}

// dropUnpublished rebuilds branch on the consensus branch without drop. It
// refuses an empty consensus branch, an empty branch and branch == upstream
// up front, then runs from the branch lock on.
func (ri *Index) dropUnpublished(ctx context.Context, branch string, drop plumbing.Hash, ownOnly, leaveOut bool, confirmed []string) (dropOutcome, error) {
	upstream := ri.UpstreamBranch()
	if upstream == "" || branch == "" || branch == upstream {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %q is not a branch to rebuild on %q", branch, upstream)
	}
	rh := ri.rh
	unlock := rh.vr.LockBranch(branch)
	defer unlock()
	localRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %s: %w", branch, err)
	}
	upRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(upstream))
	if err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %s: %w", upstream, err)
	}
	onUp := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.vr.Storer(), upRef.Hash(), nil, func(c *object.Commit) { onUp[c.Hash] = true }); err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	unpublished := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.vr.Storer(), localRef.Hash(), onUp, func(c *object.Commit) { unpublished[c.Hash] = true }); err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if !unpublished[drop] {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %s on %s: %w", drop, branch, ErrNotUnpublished)
	}
	line, unfolded, leftOut, err := rh.dropPlan(branch, localRef.Hash(), onUp, drop, ownOnly, leaveOut)
	if err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if leaveOut && !sameCommits(leftOut, confirmed) {
		return dropOutcome{}, &LeaveOutChangedError{LeaveOut: leftOut}
	}
	tempRef := plumbing.NewBranchReferenceName(branch + "-dropping")
	defer func() { _ = rh.vr.Storer().RemoveReference(tempRef) }()
	current := upRef.Hash()
	for _, st := range line {
		var others []plumbing.Hash
		if st.asMerge {
			others = st.c.ParentHashes[1:]
		}
		if current, err = rh.replayCommit(ctx, st.c, current, merge.StrategyLocalWins, others...); err != nil {
			return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: replay %s: %w", st.c.Hash, err)
		}
		if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(tempRef, current)); err != nil {
			return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
		}
	}
	ctx = context.WithoutCancel(ctx)
	if err := rh.vr.DeriveBeforeAdvance(ctx, current); err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), current)); err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if err := rh.vr.RepopulateBranch(ctx, branch); err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: repopulate: %w", err)
	}
	if err := rh.vr.NotifyCommit(ctx, branch, current); err != nil {
		return dropOutcome{}, fmt.Errorf("DropUnpublishedCommit: notify: %w", err)
	}
	log.Warn().Str("branch", branch).Str("dropped", drop.String()).Int("replayed", len(line)).Int("merges_unfolded", unfolded).Int("left_out", len(leftOut)).
		Msg("DropUnpublishedCommit: the branch was rebuilt on its upstream without the dropped commit")
	return dropOutcome{Replayed: len(line), Unfolded: unfolded, LeftOut: leftOut}, nil
}
