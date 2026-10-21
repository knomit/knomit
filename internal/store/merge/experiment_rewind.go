// An experiment open while its agent branch is rewound.
//
// A rewind (reconcileAgentRebase) rebuilds the agent branch on an upstream
// that may have scrubbed content: a force-pushed origin that removed a fact or
// a secret. Experiment branches are not rebuilt with it, so an experiment
// forked before the rewind still holds the scrubbed commits in its history. A
// plain commit would merge that history back: the scrubbed content returns in
// the tree (the three-way merge from the old common ancestor re-adds it), and
// the scrubbed commit itself becomes reachable from the agent branch, so the
// next push sends it to the origin that removed it.
//
// So the rewind RECORDS, for every experiment of the branch, the agent tip as
// it stood before the rewind (refs/knomit/experiment-rewound/<name>), and the
// next commit or sync of that experiment rebuilds it first, as ONE commit on
// the agent branch's current tip: the three-way merge a plain commit would
// make, base = merge-base(experiment tip, recorded tip) (where the experiment
// last met the old branch: its fork or its latest sync), src = the
// experiment's tip, dst = the agent branch's tip, written as a single-parent
// commit. The base and the experiment's tip agree on everything the
// experiment did not change, so what the rewind removed stays removed, and
// whatever a sync merge settled is in the experiment's tip and stays settled.
// The experiment's old commits are not parents of the result, so the scrubbed
// commit stays unreachable.
//
// THE COST: the experiment's per-commit history is squashed into that one
// commit (its message says so). Only an experiment open across a rewind pays
// it, but the rewind re-hashes the agent's own commits, so in practice an
// experiment forked after any own write of the agent is rebuilt, scrub or not.
//
// Nothing is settled silently. A path the experiment changed that the agent
// branch also changed since (an edit of a scrubbed file included) is a
// conflict: the commit refuses with the paths and the three real versions,
// and takes the caller's resolutions (and the repo's `conflicts` setting)
// exactly as a plain commit does; the sync refuses too, since its parent-wins
// rule would drop the experiment's own version for good once the old commits
// are no longer reachable. The experiment is left as it was until the rebuild
// succeeds.
//
// An experiment that held nothing the rewind removed (every commit it shares
// with the old agent branch is still on the new one) is not rebuilt: the
// record is dropped and the commit or sync proceeds as it always did.
//
// NOT COVERED: an experiment open across a rewind that ran before this record
// existed (before #538) has no record and merges as before; the pre-rewind
// tip is gone, so nothing can tell its own work from what was scrubbed.
package merge

import (
	"context"
	"errors"
	"fmt"

	"knomit/internal/store/model"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/internal/sqlite"
	"knomit/internal/store/internal/vcs"
)

// experimentRewoundRefName is where a rewind records the agent tip an
// experiment was forked from before the rewind. A bookkeeping ref, outside
// refs/heads, so the /git advertisement never lists it.
func experimentRewoundRefName(name string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/knomit/experiment-rewound/" + name)
}

// MarkExperimentsRewound records oldTip for every experiment of agentBranch
// that has no record yet. It runs BEFORE the rewind moves the branch, so a
// failure after the move cannot leave an experiment unrecorded; a record
// written for a rewind that then moves nothing is harmless (the next commit or
// sync finds nothing removed and drops it).
//
// An existing record is KEPT: the experiment's history is still told apart
// from the agent's by the branch as it stood before the FIRST rewind it lived
// through. A later tip would count the commits the first rewind removed as
// the experiment's own, since the experiment still holds them and the later
// tip does not.
//
// Caller holds LockBranch(agentBranch).
func (rh *Repo) MarkExperimentsRewound(ctx context.Context, agentBranch string, oldTip plumbing.Hash) error {
	if rh.vr.DB() == nil {
		return nil
	}
	exps, err := rh.ListExperiments(ctx)
	if err != nil {
		return err
	}
	for _, exp := range exps {
		if exp.Parent != agentBranch {
			continue
		}
		ref := experimentRewoundRefName(exp.Name)
		if _, err := rh.vr.Storer().Reference(ref); err == nil {
			continue
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("experiment %q: read rewind record: %w", exp.Name, err)
		}
		if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(ref, oldTip)); err != nil {
			return fmt.Errorf("experiment %q: write rewind record: %w", exp.Name, err)
		}
		log.Info().Str("experiment", exp.Name).Str("agent_tip", shortHash(oldTip.String())).
			Msg("experiment open across a rewind: recorded; its next commit or sync rebuilds it on the new branch")
	}
	return nil
}

// clearExperimentRewound removes the record (absent is fine).
func (rh *Repo) clearExperimentRewound(name string) error {
	err := rh.vr.Storer().RemoveReference(experimentRewoundRefName(name))
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	return nil
}

// rebuildRewoundExperiment rebuilds exp on its parent's current tip when a
// rewind recorded it (see the file comment). It returns what a `conflicts`
// setting settled. With no record, or a record that turns out to have removed
// nothing the experiment holds, it changes nothing but the record and returns
// rebuilt=false. squashed is how many of the experiment's own non-merge
// commits the rebuilt commit stands for.
//
// strategy, resolutions and o are the commit's (a sync passes StrategyRefuse
// and no resolutions); every resolution must name a path that conflicts, as
// for a plain merge.
//
// Caller holds LockBranch(exp.Parent) and LockBranch(exp.Branch()).
func (rh *Repo) rebuildRewoundExperiment(ctx context.Context, exp model.Experiment, strategy ConflictStrategy, resolutions map[string]Resolution, o MergeOpts) (rebuilt bool, squashed int, settled []SettledPath, err error) {
	rec, err := rh.vr.Storer().Reference(experimentRewoundRefName(exp.Name))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, 0, nil, nil
	}
	if err != nil {
		return false, 0, nil, fmt.Errorf("read rewind record: %w", err)
	}
	oldAgent := rec.Hash()

	expRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(exp.Branch()))
	if err != nil {
		return false, 0, nil, fmt.Errorf("experiment ref: %w", err)
	}
	parentRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(exp.Parent))
	if err != nil {
		return false, 0, nil, fmt.Errorf("parent ref: %w", err)
	}
	expTip, parentTip := expRef.Hash(), parentRef.Hash()

	// The old agent branch's history: everything the experiment holds of it
	// is the agent's, not the experiment's.
	oldHist := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.vr.Storer(), oldAgent, nil, func(c *object.Commit) { oldHist[c.Hash] = true }); err != nil {
		return false, 0, nil, fmt.Errorf("old agent history: %w", err)
	}
	// The experiment's own commits.
	ownSet := map[plumbing.Hash]bool{}
	var own []*object.Commit
	if err := vcs.WalkHistory(rh.vr.Storer(), expTip, oldHist, func(c *object.Commit) {
		own = append(own, c)
		ownSet[c.Hash] = true
	}); err != nil {
		return false, 0, nil, fmt.Errorf("experiment history: %w", err)
	}
	// Where the experiment meets the old agent branch: the parents of its own
	// commits that are not its own (the experiment's tip itself when it has
	// none). If the current branch holds every one of them it holds all they
	// reach, so nothing the rewind removed is in the experiment.
	boundary := map[plumbing.Hash]bool{}
	if len(own) == 0 {
		boundary[expTip] = true
	}
	for _, c := range own {
		if c.NumParents() == 1 {
			squashed++
		}
		for _, p := range c.ParentHashes {
			if !ownSet[p] {
				boundary[p] = true
			}
		}
	}
	parentHist := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(rh.vr.Storer(), parentTip, nil, func(c *object.Commit) { parentHist[c.Hash] = true }); err != nil {
		return false, 0, nil, fmt.Errorf("parent history: %w", err)
	}
	removed := false
	for h := range boundary {
		if !parentHist[h] {
			removed = true
			break
		}
	}
	if !removed {
		if err := rh.clearExperimentRewound(exp.Name); err != nil {
			return false, 0, nil, fmt.Errorf("clear rewind record: %w", err)
		}
		return false, 0, nil, nil
	}

	srcCommit, err := rh.vr.Repository().CommitObject(expTip)
	if err != nil {
		return false, 0, nil, fmt.Errorf("experiment commit: %w", err)
	}
	dstCommit, err := rh.vr.Repository().CommitObject(parentTip)
	if err != nil {
		return false, 0, nil, fmt.Errorf("parent commit: %w", err)
	}
	oldCommit, err := rh.vr.Repository().CommitObject(oldAgent)
	if err != nil {
		return false, 0, nil, fmt.Errorf("recorded agent commit: %w", err)
	}
	// The base a plain commit made before the rewind would have used: where
	// the experiment last met the old branch (its fork, or its latest sync).
	bases, err := srcCommit.MergeBase(oldCommit)
	if err != nil {
		return false, 0, nil, fmt.Errorf("merge base: %w", err)
	}
	if len(bases) == 0 {
		return false, 0, nil, fmt.Errorf("experiment %q shares no history with the branch it was forked from: %w", exp.Name, ErrUnrelatedHistories)
	}
	baseCommit := bases[0]

	// From here on, the three-way merge MergeIntoBranchLockedOpts makes, with
	// this base, written as a single-parent commit.
	policy, mergeFacts := ConflictsPolicyOf(strategy)
	if len(resolutions) > 0 {
		detected, err := rh.detectConflicts(ctx, baseCommit, srcCommit, dstCommit)
		if err != nil {
			return false, 0, nil, fmt.Errorf("detect conflicts: %w", err)
		}
		if err := noLeftoverResolutions(resolutions, detected); err != nil {
			return false, 0, nil, err
		}
	}
	signer, err := rh.vr.CommitSigner()
	if err != nil {
		return false, 0, nil, err
	}
	author, committer, err := rh.vr.CommitSigs(ctx, exp.Branch(), "rebuild")
	if err != nil {
		return false, 0, nil, err
	}
	var trailers []string
	res := resolutions
	if mergeFacts {
		fr, lines, err := rh.FactMergeResolutions(ctx, baseCommit, srcCommit, dstCommit, o.factMerge(policy))
		if err != nil {
			return false, 0, nil, fmt.Errorf("merge facts: %w", err)
		}
		if o.OverlayResolutions && len(resolutions) > 0 {
			if fr, lines, err = overlayCallerResolutions(fr, lines, resolutions, baseCommit, srcCommit, dstCommit); err != nil {
				return false, 0, nil, fmt.Errorf("overlay resolutions: %w", err)
			}
		}
		res, trailers = fr, lines
	}
	tree, walkLines, err := rh.MergeTreesWithStrategy(ctx, baseCommit, srcCommit, dstCommit, StrategyRefuse, res)
	if err != nil {
		var conflict *MergeConflictError
		if errors.As(err, &conflict) {
			conflict.Src, conflict.Dst = exp.Branch(), exp.Parent
			conflict.BaseCommit = baseCommit.Hash.String()
			conflict.SrcCommit = expTip.String()
			conflict.DstCommit = parentTip.String()
			conflict.Rewound = true
			log.Info().Str("experiment", exp.Name).Strs("paths", conflict.Paths).
				Msg("experiment open across a rewind: rebuild refused (conflicting paths)")
			return false, 0, nil, conflict
		}
		return false, 0, nil, fmt.Errorf("three-way merge: %w", err)
	}
	trailers = append(trailers, walkLines...)

	// An experiment that adds nothing becomes the parent's tip: no empty
	// commit.
	newTip := parentTip
	if tree != dstCommit.TreeHash {
		msg := fmt.Sprintf("experiment %s rebuilt on %s after an upstream rewind\n\n"+
			"%d commit(s) of the experiment are squashed into this one: the rewind removed\n"+
			"content their history held, and keeping them would keep it reachable. Their\n"+
			"per-commit history is not kept.", exp.Name, exp.Parent, squashed)
		nc := &object.Commit{
			Author:       author,
			Committer:    committer,
			Message:      model.AppendTrailersToParagraph(AppendTrailerLines(msg, trailers), o.Trace),
			TreeHash:     tree,
			ParentHashes: []plumbing.Hash{parentTip},
		}
		if newTip, err = vcs.StoreCommit(rh.vr.Storer(), signer, nc); err != nil {
			return false, 0, nil, err
		}
	}

	if err := rh.vr.DeriveBeforeAdvance(ctx, newTip); err != nil {
		return false, 0, nil, err
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(exp.Branch()), newTip)); err != nil {
		return false, 0, nil, fmt.Errorf("move experiment ref: %w", err)
	}
	if err := rh.vr.RepopulateBranch(ctx, exp.Branch()); err != nil {
		return false, 0, nil, fmt.Errorf("repopulate: %w", err)
	}
	if err := rh.vr.NotifyCommit(ctx, exp.Branch(), newTip); err != nil {
		return false, 0, nil, fmt.Errorf("notify: %w", err)
	}
	// The fork point moves to where the experiment now starts: "changed since
	// the fork" is its own work, not the scrubbed content it no longer holds.
	if _, err := sqlite.Conn(ctx, rh.vr.DB()).ExecContext(ctx,
		`UPDATE experiments SET fork_commit = ? WHERE name = ?`, parentTip.String(), exp.Name); err != nil {
		return false, 0, nil, fmt.Errorf("record new fork point: %w", err)
	}
	// The pipeline watermarks follow. One at the old tip (everything
	// reviewed) moves to the rebuilt tip. Any other names a commit off the new
	// chain and moves to the new fork point, so the next run reviews the whole
	// rebuilt delta rather than diffing from a commit that still holds what
	// the rewind removed.
	if _, err := sqlite.Conn(ctx, rh.vr.DB()).ExecContext(ctx, `
		UPDATE pipeline_watermarks
		SET commit_hash = CASE WHEN commit_hash = ? THEN ? ELSE ? END
		WHERE branch = ?`,
		expTip.String(), newTip.String(), parentTip.String(), exp.Branch()); err != nil {
		return false, 0, nil, fmt.Errorf("move pipeline watermarks: %w", err)
	}
	if err := rh.clearExperimentRewound(exp.Name); err != nil {
		return false, 0, nil, fmt.Errorf("clear rewind record: %w", err)
	}
	log.Info().Str("experiment", exp.Name).Str("onto", shortHash(parentTip.String())).
		Int("squashed", squashed).Str("new_tip", shortHash(newTip.String())).
		Msg("experiment open across a rewind: rebuilt on the new agent branch as one commit")
	return true, squashed, settledFrom(trailers), nil
}
