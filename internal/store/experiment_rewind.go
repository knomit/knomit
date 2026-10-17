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
// next commit or sync of that experiment rebuilds it first: the experiment's
// OWN commits (reachable from its tip, not from that recorded tip) are
// replayed one by one onto the agent branch's current tip, the same way the
// rewind replays the agent's own commits. What the experiment held of the old
// agent branch is not replayed; the agent branch already has whatever of it
// survived the rewind.
//
// Nothing is settled silently. A path the experiment changed that the agent
// branch also changed since (an edit of a scrubbed file included) is a
// conflict: the commit refuses with the paths and takes the caller's
// resolutions (and the repo's `conflicts` setting) exactly as a plain commit
// does, and the sync refuses too, since its parent-wins rule would drop the
// experiment's own version for good once the old commits are no longer
// reachable. The experiment is left as it was until the rebuild succeeds.
//
// An experiment that held nothing the rewind removed (every commit it shares
// with the old agent branch is still on the new one) is not rebuilt: the
// record is dropped and the commit or sync proceeds as it always did.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
)

// experimentRewoundRefName is where a rewind records the agent tip an
// experiment was forked from before the rewind. A bookkeeping ref, outside
// refs/heads, so the /git advertisement never lists it.
func experimentRewoundRefName(name string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/knomit/experiment-rewound/" + name)
}

// markExperimentsRewound records oldTip for every experiment of agentBranch
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
// Caller holds lockBranch(agentBranch).
func (rh *repoHandler) markExperimentsRewound(ctx context.Context, agentBranch string, oldTip plumbing.Hash) error {
	if rh.db == nil {
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
		if _, err := rh.gits.Reference(ref); err == nil {
			continue
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return fmt.Errorf("experiment %q: read rewind record: %w", exp.Name, err)
		}
		if err := rh.gits.SetReference(plumbing.NewHashReference(ref, oldTip)); err != nil {
			return fmt.Errorf("experiment %q: write rewind record: %w", exp.Name, err)
		}
		log.Info().Str("experiment", exp.Name).Str("agent_tip", shortHash(oldTip.String())).
			Msg("experiment open across a rewind: recorded; its next commit or sync rebuilds it on the new branch")
	}
	return nil
}

// clearExperimentRewound removes the record (absent is fine).
func (rh *repoHandler) clearExperimentRewound(name string) error {
	err := rh.gits.RemoveReference(experimentRewoundRefName(name))
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return err
	}
	return nil
}

// rebuildRewoundExperiment rebuilds exp on its parent's current tip when a
// rewind recorded it (see the file comment). It returns what a `conflicts`
// setting settled along the way. With no record, or a record that turns out
// to have removed nothing the experiment holds, it changes nothing but the
// record and returns rebuilt=false.
//
// strategy, resolutions and o are the commit's (a sync passes StrategyRefuse
// and no resolutions). Every resolution must name a path that conflicted in
// some step, as for a plain merge.
//
// Caller holds lockBranch(exp.Parent) and lockBranch(exp.Branch()).
func (rh *repoHandler) rebuildRewoundExperiment(ctx context.Context, exp Experiment, strategy ConflictStrategy, resolutions map[string]Resolution, o mergeOpts) (rebuilt bool, replayed int, settled []SettledPath, err error) {
	rec, err := rh.gits.Reference(experimentRewoundRefName(exp.Name))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, 0, nil, nil
	}
	if err != nil {
		return false, 0, nil, fmt.Errorf("read rewind record: %w", err)
	}
	oldAgent := rec.Hash()

	expRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(exp.Branch()))
	if err != nil {
		return false, 0, nil, fmt.Errorf("experiment ref: %w", err)
	}
	parentRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(exp.Parent))
	if err != nil {
		return false, 0, nil, fmt.Errorf("parent ref: %w", err)
	}
	expTip, parentTip := expRef.Hash(), parentRef.Hash()

	// The old agent branch's history: everything the experiment holds of it
	// is the agent's, not the experiment's.
	oldHist := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, oldAgent, nil, func(c *object.Commit) { oldHist[c.Hash] = true }); err != nil {
		return false, 0, nil, fmt.Errorf("old agent history: %w", err)
	}
	// The experiment's own commits, parents first.
	var own []*object.Commit
	ownSet := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, expTip, oldHist, func(c *object.Commit) {
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
		for _, p := range c.ParentHashes {
			if !ownSet[p] {
				boundary[p] = true
			}
		}
	}
	parentHist := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, parentTip, nil, func(c *object.Commit) { parentHist[c.Hash] = true }); err != nil {
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

	signer, err := rh.commitSigner()
	if err != nil {
		return false, 0, nil, err
	}
	_, committer, err := rh.commitSigs(ctx, exp.Branch(), "rebuild")
	if err != nil {
		return false, 0, nil, err
	}
	policy, mergeFacts := conflictsPolicyOf(strategy)

	cur, err := rh.repo.CommitObject(parentTip)
	if err != nil {
		return false, 0, nil, fmt.Errorf("parent commit: %w", err)
	}
	detectedAll := map[string]bool{}
	unresolved := map[string]bool{}
	var trailersAll []string
	for _, orig := range own {
		// A merge of the experiment's (a sync) is not replayed: what it
		// brought is the old agent branch's. The own commits above it carry
		// their own change against it, which is what is replayed.
		if orig.NumParents() != 1 {
			continue
		}
		base, err := orig.Parents().Next()
		if err != nil {
			return false, 0, nil, fmt.Errorf("parent of %s: %w", orig.Hash, err)
		}
		detected, err := rh.detectConflicts(ctx, base, orig, cur)
		if err != nil {
			return false, 0, nil, fmt.Errorf("replay %s: %w", shortHash(orig.Hash.String()), err)
		}
		for p := range detected {
			detectedAll[p] = true
		}
		var res map[string]Resolution
		var lines []string
		if mergeFacts {
			if res, lines, err = rh.factMergeResolutions(ctx, base, orig, cur, o.factMerge(policy)); err != nil {
				return false, 0, nil, fmt.Errorf("replay %s: merge facts: %w", shortHash(orig.Hash.String()), err)
			}
		}
		caller := map[string]Resolution{}
		for p, r := range resolutions {
			if detected[p] {
				caller[p] = r
			}
		}
		if len(caller) > 0 {
			if mergeFacts && o.overlayResolutions {
				if res, lines, err = overlayCallerResolutions(res, lines, caller, base, orig, cur); err != nil {
					return false, 0, nil, fmt.Errorf("replay %s: overlay resolutions: %w", shortHash(orig.Hash.String()), err)
				}
			} else if !mergeFacts {
				res = caller
			}
		}
		// What is still unresolved is collected, and the walk goes on with
		// the experiment's version of it, so every conflicting path of every
		// step is reported at once; nothing is written to a ref unless none is
		// left.
		step := map[string]Resolution{}
		for p, r := range res {
			step[p] = r
		}
		for p := range detected {
			if _, ok := step[p]; !ok {
				unresolved[p] = true
				step[p] = Resolution{Side: ResolveSrc}
			}
		}
		tree, walkLines, err := rh.mergeTreesWithStrategy(ctx, base, orig, cur, StrategyRefuse, step)
		if err != nil {
			return false, 0, nil, fmt.Errorf("replay %s: %w", shortHash(orig.Hash.String()), err)
		}
		lines = append(lines, walkLines...)
		trailersAll = append(trailersAll, lines...)
		nc := &object.Commit{
			Author:       orig.Author,
			Committer:    committer,
			Message:      appendTrailerLines(orig.Message, lines),
			TreeHash:     tree,
			ParentHashes: []plumbing.Hash{cur.Hash},
		}
		replayed++
		h, err := storeCommit(rh.gits, signer, nc)
		if err != nil {
			return false, 0, nil, fmt.Errorf("replay %s: %w", shortHash(orig.Hash.String()), err)
		}
		if cur, err = rh.repo.CommitObject(h); err != nil {
			return false, 0, nil, err
		}
	}
	if err := noLeftoverResolutions(resolutions, detectedAll); err != nil {
		return false, 0, nil, err
	}
	if len(unresolved) > 0 {
		return false, 0, nil, rh.rewoundConflict(exp, unresolved, expTip, parentTip, oldAgent)
	}

	if err := rh.deriveBeforeAdvance(ctx, cur.Hash); err != nil {
		return false, 0, nil, err
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(exp.Branch()), cur.Hash)); err != nil {
		return false, 0, nil, fmt.Errorf("move experiment ref: %w", err)
	}
	if err := rh.repopulateBranch(ctx, exp.Branch()); err != nil {
		return false, 0, nil, fmt.Errorf("repopulate: %w", err)
	}
	if err := rh.notifyCommit(ctx, exp.Branch(), cur.Hash); err != nil {
		return false, 0, nil, fmt.Errorf("notify: %w", err)
	}
	// The fork point moves to where the experiment now starts: "changed since
	// the fork" is its own work, not the scrubbed content it no longer holds.
	if _, err := conn(ctx, rh.db).ExecContext(ctx,
		`UPDATE experiments SET fork_commit = ? WHERE name = ?`, parentTip.String(), exp.Name); err != nil {
		return false, 0, nil, fmt.Errorf("record new fork point: %w", err)
	}
	if err := rh.clearExperimentRewound(exp.Name); err != nil {
		return false, 0, nil, fmt.Errorf("clear rewind record: %w", err)
	}
	log.Info().Str("experiment", exp.Name).Str("onto", shortHash(parentTip.String())).
		Int("replayed", replayed).Str("new_tip", shortHash(cur.Hash.String())).
		Msg("experiment open across a rewind: rebuilt on the new agent branch")
	return true, replayed, settledFrom(trailersAll), nil
}

// rewoundConflict is the refusal of a rebuild: the paths, and the three
// commits to read each at. The base is where the experiment last met the old
// agent branch (its fork, or its latest sync), so it is the version a plain
// commit would have used as its base.
func (rh *repoHandler) rewoundConflict(exp Experiment, paths map[string]bool, expTip, parentTip, oldAgent plumbing.Hash) error {
	ce := &MergeConflictError{Src: exp.Branch(), Dst: exp.Parent, SrcCommit: expTip.String(), DstCommit: parentTip.String(), Rewound: true}
	for p := range paths {
		ce.Paths = append(ce.Paths, p)
	}
	sort.Strings(ce.Paths)
	ec, err := rh.repo.CommitObject(expTip)
	if err != nil {
		return err
	}
	oc, err := rh.repo.CommitObject(oldAgent)
	if err != nil {
		return err
	}
	if bases, err := ec.MergeBase(oc); err == nil && len(bases) > 0 {
		ce.BaseCommit = bases[0].Hash.String()
		if bt, err := bases[0].Tree(); err == nil {
			for _, p := range ce.Paths {
				if _, has := treeBlobHash(bt, p); !has {
					ce.PathsWithoutBase = append(ce.PathsWithoutBase, p)
				}
			}
		}
	}
	log.Info().Str("experiment", exp.Name).Strs("paths", ce.Paths).
		Msg("experiment open across a rewind: rebuild refused (conflicting paths)")
	return ce
}
