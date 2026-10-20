package remotesync

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
)

// DropOutcome is what DropUnpublished did: how many commits it replayed, how
// many merges it unfolded, and what it left out (the confirmed fallback).
type DropOutcome struct {
	Replayed, Unfolded int
	LeftOut            []LostCommit
}

// DropUnpublished rebuilds branch on upstream without drop; see
// Service.DropUnpublishedCommit for the contract. The caller (the Service)
// has already refused an empty upstream, an empty branch and branch ==
// upstream. This is the body of the former Service.dropUnpublished from the
// branch lock on, statement for statement.
func (rh *Repo) DropUnpublished(ctx context.Context, upstream, branch string, drop plumbing.Hash, ownOnly, leaveOut bool, confirmed []string) (DropOutcome, error) {
	unlock := rh.LockBranch(branch)
	defer unlock()
	localRef, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %s: %w", branch, err)
	}
	upRef, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(upstream))
	if err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %s: %w", upstream, err)
	}
	onUp := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.Storer(), upRef.Hash(), nil, func(c *object.Commit) { onUp[c.Hash] = true }); err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	unpublished := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.Storer(), localRef.Hash(), onUp, func(c *object.Commit) { unpublished[c.Hash] = true }); err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if !unpublished[drop] {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %s on %s: %w", drop, branch, ErrNotUnpublished)
	}
	line, unfolded, leftOut, err := rh.dropPlan(branch, localRef.Hash(), onUp, drop, ownOnly, leaveOut)
	if err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if leaveOut && !sameCommits(leftOut, confirmed) {
		return DropOutcome{}, &LeaveOutChangedError{LeaveOut: leftOut}
	}
	tempRef := plumbing.NewBranchReferenceName(branch + "-dropping")
	defer func() { _ = rh.Storer().RemoveReference(tempRef) }()
	current := upRef.Hash()
	for _, st := range line {
		var others []plumbing.Hash
		if st.asMerge {
			others = st.c.ParentHashes[1:]
		}
		if current, err = rh.replayCommit(ctx, st.c, current, merge.StrategyLocalWins, others...); err != nil {
			return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: replay %s: %w", st.c.Hash, err)
		}
		if err := rh.Storer().SetReference(plumbing.NewHashReference(tempRef, current)); err != nil {
			return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
		}
	}
	ctx = context.WithoutCancel(ctx)
	if err := rh.DeriveBeforeAdvance(ctx, current); err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), current)); err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %w", err)
	}
	if err := rh.RepopulateBranch(ctx, branch); err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: repopulate: %w", err)
	}
	if err := rh.NotifyCommit(ctx, branch, current); err != nil {
		return DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: notify: %w", err)
	}
	log.Warn().Str("branch", branch).Str("dropped", drop.String()).Int("replayed", len(line)).Int("merges_unfolded", unfolded).Int("left_out", len(leftOut)).
		Msg("DropUnpublishedCommit: the branch was rebuilt on its upstream without the dropped commit")
	return DropOutcome{Replayed: len(line), Unfolded: unfolded, LeftOut: leftOut}, nil
}
