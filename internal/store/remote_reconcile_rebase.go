// Rebase-fallback machinery for the force-rewind case. The merge-based
// reconcile in remote_reconcile_merge.go (added in Task 3) cannot make
// progress when local main was force-updated to a disjoint history
// (origin/main rewind): there is no common ancestor between agent and
// main, so a three-way merge has nothing to anchor on. In that case
// reconcileAgent routes here, which walks the agent's local-only commits
// (since the watermark) and replays them onto the new main using
// replayCommit. The watermark stops the walk at the last-consumed main
// commit, so files scrubbed by the rewind don't get resurrected.
//
// In steady-state operation (no rewind), this file is never entered.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	storegit "knomit/internal/store/git"
)

// unpushedCommits returns commits reachable from localTip but not from
// upstreamTip, ordered oldest → newest (the order in which they should be
// replayed). The walk follows first-parent ancestry only — merge commits
// take their "ours" side, matching the linear-history goal.
//
// When explicitBase is non-zero, it is used directly as the walk's stop
// point (the watermark — "the main commit this agent last consumed").
// disjoint is reported false when explicitBase is set, because the
// caller (reconcileAgent) wants the linear "since last main" delta even
// when local and the new upstream are unrelated.
//
// When explicitBase is zero, the stop point is computed from MergeBase:
// disjoint=true when localTip and upstreamTip share no common ancestor
// (every local commit back to root is replayed onto upstreamTip),
// disjoint=false otherwise.
//
// Returns empty (and disjoint=false) when localTip is an ancestor of
// upstreamTip (nothing local to replay — caller will fast-forward), OR
// when upstreamTip is an ancestor of localTip (local is a strict linear
// extension — caller's force-push will fast-forward origin without
// rewriting commit hashes).
func (rh *repoHandler) unpushedCommits(localTip, upstreamTip, explicitBase plumbing.Hash) ([]*object.Commit, bool, error) {
	if localTip == upstreamTip {
		return nil, false, nil
	}

	local, err := rh.repo.CommitObject(localTip)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: local commit %s: %w", localTip, err)
	}
	upstream, err := rh.repo.CommitObject(upstreamTip)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: upstream commit %s: %w", upstreamTip, err)
	}

	// If local is an ancestor of upstream, nothing to replay.
	isAncestor, err := local.IsAncestor(upstream)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: IsAncestor (local→upstream): %w", err)
	}
	if isAncestor {
		return nil, false, nil
	}

	// If upstream is an ancestor of local, local is a strict linear extension
	// of upstream — no replay needed. The caller's force-push will push the
	// existing local commits as a fast-forward on origin (no hash rewrite).
	upstreamAncestor, err := upstream.IsAncestor(local)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: IsAncestor (upstream→local): %w", err)
	}
	if upstreamAncestor {
		return nil, false, nil
	}

	var stopAt plumbing.Hash
	disjoint := false
	if explicitBase != plumbing.ZeroHash {
		// Caller supplied a watermark — use it directly. We trust the caller
		// to have established that explicitBase is reachable from localTip
		// (it was a former tip of either local main or local agent).
		stopAt = explicitBase
	} else {
		bases, err := local.MergeBase(upstream)
		if err != nil {
			return nil, false, fmt.Errorf("unpushedCommits: MergeBase: %w", err)
		}
		if len(bases) == 0 {
			disjoint = true
			// Walk all the way back to root.
			stopAt = plumbing.ZeroHash
		} else {
			stopAt = bases[0].Hash
		}
	}

	// Walk first-parent from local back to (but not including) stopAt.
	var collected []*object.Commit
	cur := local
	for {
		if cur.Hash == stopAt {
			break
		}
		collected = append(collected, cur)
		if cur.NumParents() == 0 {
			break
		}
		parent, err := cur.Parents().Next()
		if err != nil {
			return nil, false, fmt.Errorf("unpushedCommits: walk first-parent at %s: %w", cur.Hash, err)
		}
		cur = parent
	}

	// Reverse to oldest-first.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, disjoint, nil
}

// replayCommit synthesizes a new commit by applying orig's tree-delta
// (orig vs orig's first parent) on top of ontoHash, with conflictStrategy
// resolving overlapping changes. The returned hash is signed.
//
// Preserves orig.Author, orig.Author.When, and orig.Message. Committer is
// the knomit signer; committer.When is now. ParentHashes is [ontoHash] —
// single parent, producing linear history.
//
// For a root commit (orig has no parents), the "base" is the empty tree;
// the merge becomes "apply every file from orig.TreeHash onto ontoTree
// with strategy resolving conflicts on overlapping paths".
//
// The strategy parameter is interpreted from the replay CALLER's
// perspective (agent-machine vs origin-server):
//
//   - StrategyLocalWins  → the agent's (orig's) content wins overlapping
//     conflicts. This is the default for origin sync — the agent's local
//     edits are preserved when both sides modified the same path.
//   - StrategyRemoteWins → the upstream's (ontoCommit's) content wins.
//
// Note the inversion versus mergeTreesWithStrategy: that helper's
// "Local/Remote" refers to dst/src (merge-target/merge-source), while
// the user-facing replay framing here refers to agent/origin. Inside
// replayCommit, orig is passed as src and ontoCommit as dst, so the
// two namings are opposites. We translate at the boundary so callers
// pass the strategy they actually mean.
func (rh *repoHandler) replayCommit(
	ctx context.Context,
	orig *object.Commit,
	ontoHash plumbing.Hash,
	strategy ConflictStrategy,
) (plumbing.Hash, error) {
	ontoCommit, err := rh.repo.CommitObject(ontoHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: onto commit %s: %w", ontoHash, err)
	}

	// Determine the "base" side of the three-way merge: orig's first parent
	// if it has one; an empty synthetic commit otherwise.
	var baseCommit *object.Commit
	if orig.NumParents() > 0 {
		parent, err := orig.Parents().Next()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: orig parent: %w", err)
		}
		baseCommit = parent
	} else {
		// Synthesize an empty-tree commit as the base. mergeTreesWithStrategy
		// only reads .Tree() from baseCommit; an empty tree means every file
		// in orig.TreeHash registers as an Insert (non-conflicting), which is
		// the correct semantic for replaying a root commit.
		//
		// IMPORTANT: object.Commit.Tree() reads through the commit's private
		// storer field; a struct literal would leave that field nil and
		// crash on .Tree(). Storing a synthetic commit and rehydrating via
		// object.GetCommit attaches the storer.
		emptyTreeHash, err := storeEmptyTree(rh.gits)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: empty tree: %w", err)
		}
		synth := &object.Commit{
			Author:    object.Signature{Name: "knomit", Email: "knomit@local", When: timeNow()},
			Committer: object.Signature{Name: "knomit", Email: "knomit@local", When: timeNow()},
			Message:   "synthetic empty-tree base",
			TreeHash:  emptyTreeHash,
		}
		enc := rh.gits.NewEncodedObject()
		if err := synth.Encode(enc); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: encode synthetic base: %w", err)
		}
		synthHash, err := rh.gits.SetEncodedObject(enc)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: store synthetic base: %w", err)
		}
		baseCommit, err = object.GetCommit(rh.gits, synthHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: rehydrate synthetic base: %w", err)
		}
	}

	// Strategy translation: in the user-facing replay framing, "Local"
	// refers to this machine's agent (which IS the src commit being
	// replayed). In mergeTreesWithStrategy's framing, "Local" = dst (the
	// side being updated, which is ontoCommit/upstream here). The two
	// "Local"s are OPPOSITES. Invert when calling the merge helper so
	// callers can pass the strategy they actually mean. (See replayCommit
	// doc comment.)
	var mergeStrategy ConflictStrategy
	switch strategy {
	case StrategyLocalWins:
		mergeStrategy = StrategyRemoteWins
	case StrategyRemoteWins:
		mergeStrategy = StrategyLocalWins
	default:
		// Empty / unrecognized: default to agent-wins (the project decision
		// for origin sync replay).
		mergeStrategy = StrategyRemoteWins
	}

	// Three-way merge: base = baseCommit (orig's parent or empty),
	//                  src  = orig (what orig adds),
	//                  dst  = ontoCommit (what we're replaying on top of).
	mergedTreeHash, err := rh.mergeTreesWithStrategy(ctx, baseCommit, orig, ontoCommit, mergeStrategy)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: three-way merge: %w", err)
	}

	newCommit := &object.Commit{
		Author: orig.Author, // includes original When
		Committer: object.Signature{
			Name:  "knomit",
			Email: "knomit@local",
			When:  timeNow(),
		},
		Message:      orig.Message,
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{ontoHash},
	}

	enc := rh.gits.NewEncodedObject()
	if err := newCommit.Encode(enc); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: encode: %w", err)
	}
	h, err := rh.gits.SetEncodedObject(enc)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: store: %w", err)
	}
	h, err = signCommitInPlace(rh.gits, rh.signer, h)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: sign: %w", err)
	}
	return h, nil
}

// storeEmptyTree stores (or retrieves) the empty tree object and returns
// its hash. go-git's empty-tree hash is well-known but storing explicitly
// keeps the storer consistent across encoder backends.
func storeEmptyTree(s *storegit.Storer) (plumbing.Hash, error) {
	emptyTree := &object.Tree{}
	enc := s.NewEncodedObject()
	if err := emptyTree.Encode(enc); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.SetEncodedObject(enc)
}

// timeNow is a var so tests can stub it. Otherwise wraps time.Now().
var timeNow = func() time.Time { return time.Now() }

// replayOntoUpstream reconciles localBranch with upstreamTip by replaying
// localBranch's unpushed commits onto upstreamTip. Uses local-wins
// conflict resolution for overlapping paths (agent-facing semantics —
// see replayCommit for the strategy translation).
//
// When explicitBase is non-zero, it is passed to unpushedCommits as the
// walk's stop point — the "this is how far back to consider commits
// unpushed" marker. reconcileAgent uses this to anchor the walk at the
// last-consumed main commit (the watermark) rather than at the merge
// base, which is what allows main-side updates to flow into the agent.
// When zero, unpushedCommits uses MergeBase as before.
//
// Atomicity: replayed commits accumulate under a temporary ref
// (refs/heads/<localBranch>-replaying). Only when the full chain succeeds
// does localBranch advance to the new tip. The temp ref is always removed
// before returning, on both success and failure paths; localBranch is
// unchanged on failure.
//
// Caller must hold rh.lockBranch(localBranch).
func (rh *repoHandler) replayOntoUpstream(
	ctx context.Context,
	localBranch string,
	upstreamTip plumbing.Hash,
	explicitBase plumbing.Hash,
	strategy ConflictStrategy,
) (AgentReconcileResult, error) {
	localRefName := plumbing.NewBranchReferenceName(localBranch)
	localRef, err := rh.gits.Reference(localRefName)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: local ref %q: %w", localBranch, err)
	}
	localTip := localRef.Hash()

	commits, disjoint, err := rh.unpushedCommits(localTip, upstreamTip, explicitBase)
	if err != nil {
		return AgentReconcileResult{}, err
	}

	// No unpushed commits. unpushedCommits returns empty in two distinct
	// ancestry shapes:
	//   1. local is an ancestor of upstream → fast-forward local to upstream.
	//   2. upstream is an ancestor of local → local is a strict linear
	//      extension; the caller's force-push will fast-forward origin.
	//      Do NOT rewind local — that would orphan its branch_commits rows.
	if len(commits) == 0 {
		if localTip == upstreamTip {
			return AgentReconcileResult{Mode: "noop"}, nil
		}
		localCommit, err := rh.repo.CommitObject(localTip)
		if err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: local commit: %w", err)
		}
		upstreamCommit, err := rh.repo.CommitObject(upstreamTip)
		if err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: upstream commit: %w", err)
		}
		isLocalAncestor, err := localCommit.IsAncestor(upstreamCommit)
		if err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: IsAncestor: %w", err)
		}
		if !isLocalAncestor {
			// Local is strictly ahead of upstream — no replay, no fast-forward.
			// Caller's force-push will advance origin to local.
			return AgentReconcileResult{Mode: "noop", NewTip: localTip.String()}, nil
		}
		newRef := plumbing.NewHashReference(localRefName, upstreamTip)
		if err := rh.gits.SetReference(newRef); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: fast-forward: %w", err)
		}
		if err := rh.populateCommitLog(ctx, localBranch); err != nil {
			log.Warn().Err(err).Msg("replayOntoUpstream: populate after FF")
		}
		if err := rh.notifyCommit(ctx, localBranch, upstreamTip); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: notify after FF: %w", err)
		}
		return AgentReconcileResult{Mode: "ff", FastForward: true, NewTip: upstreamTip.String()}, nil
	}

	log.Info().
		Str("branch", localBranch).
		Int("count", len(commits)).
		Bool("disjoint", disjoint).
		Str("upstream", upstreamTip.String()[:8]).
		Msg("replayOntoUpstream: starting")

	tempRefName := plumbing.NewBranchReferenceName(localBranch + "-replaying")
	// Start temp ref at upstream tip.
	if err := rh.gits.SetReference(plumbing.NewHashReference(tempRefName, upstreamTip)); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: init temp ref: %w", err)
	}
	// Always remove the temp ref before returning — success or failure —
	// so the branch namespace doesn't accumulate stale "-replaying" refs.
	// The check via Reference() guards against double-removal noise if a
	// future code path drops the ref explicitly before returning.
	defer func() {
		if _, err := rh.gits.Reference(tempRefName); err == nil {
			if rmErr := rh.gits.RemoveReference(tempRefName); rmErr != nil {
				log.Warn().Err(rmErr).Msg("replayOntoUpstream: remove temp ref (continuing)")
			}
		}
	}()

	current := upstreamTip
	for i, orig := range commits {
		newHash, err := rh.replayCommit(ctx, orig, current, strategy)
		if err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: replay step %d/%d (%s): %w",
				i+1, len(commits), orig.Hash.String()[:8], err)
		}
		current = newHash
		// Advance temp ref so each replayed commit is reachable.
		if err := rh.gits.SetReference(plumbing.NewHashReference(tempRefName, current)); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: advance temp ref: %w", err)
		}
	}

	// Atomic move: agent ref → new tip. Temp ref is cleaned up by the
	// deferred removal above.
	if err := rh.gits.SetReference(plumbing.NewHashReference(localRefName, current)); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: atomic move: %w", err)
	}

	// The pre-replay chain is no longer reachable from localBranch. Purge
	// stale branch_commits rows so populateCommitLog can rebuild parity from
	// the new tip without Verify complaining about unreachable rows. (Same
	// pattern as reconcileMain's rewind path.) We always reach this with
	// len(commits) > 0 (the no-commits case returned earlier), so the purge
	// is unconditional.
	if err := rh.purgeBranchCommits(ctx, localBranch); err != nil {
		log.Warn().Err(err).Msg("replayOntoUpstream: purge branch_commits")
	}
	if err := rh.populateCommitLog(ctx, localBranch); err != nil {
		log.Warn().Err(err).Msg("replayOntoUpstream: populate")
	}
	if err := rh.notifyCommit(ctx, localBranch, current); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: notify: %w", err)
	}

	log.Info().
		Str("branch", localBranch).
		Int("count", len(commits)).
		Str("new_tip", current.String()[:8]).
		Msg("replayOntoUpstream: complete")

	return AgentReconcileResult{
		Mode:        "rebase",
		Replayed:    true,
		NumReplayed: len(commits),
		NewTip:      current.String(),
	}, nil
}
