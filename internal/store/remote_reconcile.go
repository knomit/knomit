// Origin reconciliation primitives. The agent branch is reconciled by
// replaying its unpushed commits onto local main (a consensus mirror).
// origin/agent/<host> is a push target only — the agent no longer reads
// from it after bootstrap. A per-branch watermark
// (refs/knomit/agent-base/<branch>) records the main commit the agent
// last consumed, which acts as the replay base for unpushedCommits.
//
// The reconcile primitives are the single source of truth for "what
// does sync do" — Sync/Push/InitFromRemote/ActivateSync all call into
// them.
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

// agentBaseRefName returns the watermark ref name for an agent branch.
// The watermark lives under refs/knomit/ to keep it out of the regular
// refs/heads/ branch listing (and out of any "show me the branches" UI).
func agentBaseRefName(agentBranch string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/knomit/agent-base/" + agentBranch)
}

// readAgentBase returns the hash recorded in the watermark for agentBranch.
// Returns plumbing.ErrReferenceNotFound (wrapped) when the watermark has
// never been written for this branch.
func (rh *repoHandler) readAgentBase(agentBranch string) (plumbing.Hash, error) {
	ref, err := rh.gits.Reference(agentBaseRefName(agentBranch))
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// writeAgentBase updates the watermark for agentBranch to hash. The
// watermark identifies "the main commit this agent last consumed" and
// is read on the next Sync tick as the base for unpushedCommits.
func (rh *repoHandler) writeAgentBase(agentBranch string, hash plumbing.Hash) error {
	return rh.gits.SetReference(plumbing.NewHashReference(agentBaseRefName(agentBranch), hash))
}

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

// ReplayOntoUpstreamResult reports what replayOntoUpstream did.
type ReplayOntoUpstreamResult struct {
	Replayed    bool   // true if any new commits were synthesized
	NumReplayed int    // number of original commits replayed
	FastForward bool   // true if local ref was advanced to upstream without replay
	NewTip      string // hash of the new agent tip (empty when no-op)
}

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
// does localBranch advance to the new tip and the temp ref get removed.
// On failure, the temp ref is left behind for inspection and localBranch
// is unchanged.
//
// Caller must hold rh.lockBranch(localBranch).
func (rh *repoHandler) replayOntoUpstream(
	ctx context.Context,
	localBranch string,
	upstreamTip plumbing.Hash,
	explicitBase plumbing.Hash,
	strategy ConflictStrategy,
) (ReplayOntoUpstreamResult, error) {
	localRefName := plumbing.NewBranchReferenceName(localBranch)
	localRef, err := rh.gits.Reference(localRefName)
	if err != nil {
		return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: local ref %q: %w", localBranch, err)
	}
	localTip := localRef.Hash()

	commits, disjoint, err := rh.unpushedCommits(localTip, upstreamTip, explicitBase)
	if err != nil {
		return ReplayOntoUpstreamResult{}, err
	}

	// No unpushed commits. unpushedCommits returns empty in two distinct
	// ancestry shapes:
	//   1. local is an ancestor of upstream → fast-forward local to upstream.
	//   2. upstream is an ancestor of local → local is a strict linear
	//      extension; the caller's force-push will fast-forward origin.
	//      Do NOT rewind local — that would orphan its branch_commits rows.
	if len(commits) == 0 {
		if localTip == upstreamTip {
			return ReplayOntoUpstreamResult{}, nil
		}
		localCommit, err := rh.repo.CommitObject(localTip)
		if err != nil {
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: local commit: %w", err)
		}
		upstreamCommit, err := rh.repo.CommitObject(upstreamTip)
		if err != nil {
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: upstream commit: %w", err)
		}
		isLocalAncestor, err := localCommit.IsAncestor(upstreamCommit)
		if err != nil {
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: IsAncestor: %w", err)
		}
		if !isLocalAncestor {
			// Local is strictly ahead of upstream — no replay, no fast-forward.
			// Caller's force-push will advance origin to local.
			return ReplayOntoUpstreamResult{NewTip: localTip.String()}, nil
		}
		newRef := plumbing.NewHashReference(localRefName, upstreamTip)
		if err := rh.gits.SetReference(newRef); err != nil {
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: fast-forward: %w", err)
		}
		if err := rh.populateCommitLog(ctx, localBranch); err != nil {
			log.Warn().Err(err).Msg("replayOntoUpstream: populate after FF")
		}
		if err := rh.notifyCommit(ctx, localBranch, upstreamTip); err != nil {
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: notify after FF: %w", err)
		}
		return ReplayOntoUpstreamResult{FastForward: true, NewTip: upstreamTip.String()}, nil
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
		return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: init temp ref: %w", err)
	}

	current := upstreamTip
	for i, orig := range commits {
		newHash, err := rh.replayCommit(ctx, orig, current, strategy)
		if err != nil {
			// Leave temp ref behind for inspection. Agent ref untouched.
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: replay step %d/%d (%s): %w",
				i+1, len(commits), orig.Hash.String()[:8], err)
		}
		current = newHash
		// Advance temp ref so each replayed commit is reachable.
		if err := rh.gits.SetReference(plumbing.NewHashReference(tempRefName, current)); err != nil {
			return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: advance temp ref: %w", err)
		}
	}

	// Atomic move: agent ref → new tip. Then drop temp ref.
	if err := rh.gits.SetReference(plumbing.NewHashReference(localRefName, current)); err != nil {
		return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: atomic move: %w", err)
	}
	if err := rh.gits.RemoveReference(tempRefName); err != nil {
		log.Warn().Err(err).Msg("replayOntoUpstream: remove temp ref (continuing)")
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
		return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: notify: %w", err)
	}

	log.Info().
		Str("branch", localBranch).
		Int("count", len(commits)).
		Str("new_tip", current.String()[:8]).
		Msg("replayOntoUpstream: complete")

	return ReplayOntoUpstreamResult{
		Replayed:    true,
		NumReplayed: len(commits),
		NewTip:      current.String(),
	}, nil
}

// MainReconcileResult reports the outcome of reconcileMain.
type MainReconcileResult struct {
	FastForward bool   // local main was advanced to origin/main
	Rewound     bool   // origin/main was not a descendant of local main — force-updated
	NewTip      string // hash of the new local main tip (empty when no-op)
}

// reconcileMain updates local main to track origin/main. Fast-forwards
// when origin/main is a descendant of local main. When origin/main is
// NOT a descendant (rewind, force-push, or disjoint history on the
// remote), force-updates local main and reports Rewound=true — the
// caller must then re-migrate the agent branch against the new main.
//
// Errors if origin/main is not present locally (caller must fetch first).
//
// Caller must hold rh.lockBranch("main"). After every ref advance,
// commit_log is repopulated and the index manager is notified so
// downstream readers see consistent state.
func (rh *repoHandler) reconcileMain(ctx context.Context) (MainReconcileResult, error) {
	originMainName := plumbing.NewRemoteReferenceName("origin", "main")
	originMainRef, err := rh.gits.Reference(originMainName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: read origin/main: %w", err)
	}
	originHash := originMainRef.Hash()

	localMainName := plumbing.NewBranchReferenceName("main")
	localMainRef, err := rh.gits.Reference(localMainName)
	if err != nil {
		// Local main doesn't exist — create at origin/main.
		if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: create local main: %w", err)
		}
		if _, err := rh.EnsureBranch(ctx, "main", "refs/heads/main"); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: ensure main: %w", err)
		}
		if err := rh.populateCommitLog(ctx, "main"); err != nil {
			log.Warn().Err(err).Msg("reconcileMain: populate commit_log after create")
		}
		if err := rh.notifyCommit(ctx, "main", originHash); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after create: %w", err)
		}
		return MainReconcileResult{FastForward: true, NewTip: originHash.String()}, nil
	}
	localHash := localMainRef.Hash()

	if localHash == originHash {
		return MainReconcileResult{NewTip: originHash.String()}, nil
	}

	localCommit, err := rh.repo.CommitObject(localHash)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: local commit: %w", err)
	}
	originCommit, err := rh.repo.CommitObject(originHash)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: origin commit: %w", err)
	}

	isLocalAncestor, err := localCommit.IsAncestor(originCommit)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: IsAncestor: %w", err)
	}
	if isLocalAncestor {
		// Fast-forward.
		if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: fast-forward: %w", err)
		}
		if err := rh.populateCommitLog(ctx, "main"); err != nil {
			log.Warn().Err(err).Msg("reconcileMain: populate commit_log after fast-forward")
		}
		if err := rh.notifyCommit(ctx, "main", originHash); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after fast-forward: %w", err)
		}
		log.Info().Str("to", originHash.String()[:8]).Msg("reconcileMain: fast-forward")
		return MainReconcileResult{FastForward: true, NewTip: originHash.String()}, nil
	}

	// origin/main is not a descendant of local main → rewind / divergent advance.
	// Force-update local main; caller is responsible for re-migrating the agent.
	if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: force-update: %w", err)
	}
	// The old chain is no longer reachable from main. Purge stale
	// branch_commits rows before repopulating; otherwise Verify reports
	// unreachable rows because populateCommitLog only INSERTs.
	if err := rh.purgeBranchCommits(ctx, "main"); err != nil {
		log.Warn().Err(err).Msg("reconcileMain: purge branch_commits after rewind")
	}
	if err := rh.populateCommitLog(ctx, "main"); err != nil {
		log.Warn().Err(err).Msg("reconcileMain: populate commit_log after force-update")
	}
	if err := rh.notifyCommit(ctx, "main", originHash); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after force-update: %w", err)
	}
	log.Warn().
		Str("local", localHash.String()[:8]).
		Str("origin", originHash.String()[:8]).
		Msg("reconcileMain: origin/main is not a descendant of local main; force-updated")
	return MainReconcileResult{Rewound: true, NewTip: originHash.String()}, nil
}

// purgeBranchCommits deletes every branch_commits row for the given branch.
// Used by reconcileMain on a rewind so populateCommitLog can repopulate from
// the new HEAD without leaving stranded rows for commits that are no longer
// reachable.
func (rh *repoHandler) purgeBranchCommits(ctx context.Context, branch string) error {
	id, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("purgeBranchCommits: branchID: %w", err)
	}
	if _, err := rh.db.ExecContext(ctx, `DELETE FROM branch_commits WHERE branch_id = ?`, id); err != nil {
		return fmt.Errorf("purgeBranchCommits: delete: %w", err)
	}
	return nil
}

// reconcileAgent reconciles agentBranch by replaying its unpushed commits
// (those since the watermark) onto current local main. Conflict resolution
// uses the supplied strategy (agent-facing semantics — see replayCommit).
//
// The "upstream" for the agent is now local main (which reconcileMain has
// already aligned to origin/main). origin/agent/<host> is a push target
// only; the agent never reads from it after bootstrap.
//
// A per-branch watermark (refs/knomit/agent-base/<branch>) records the
// main commit the agent last consumed. unpushedCommits uses the
// watermark as its base, so:
//
//   - Forward main advances merge cleanly into the agent (the watermark
//     stays behind main; commits before main and after the watermark
//     don't exist on the agent, so the agent fast-forwards or replays
//     local-only commits on top of the new main).
//   - Forward main deletions correctly drop files from the agent (the
//     fast-forward picks up the deletion).
//   - Main force-push rewinds drop scrubbed files from the agent: when
//     the watermark equals the old main and the agent has no local
//     commits since it, unpushedCommits returns empty and the agent
//     fast-forwards onto the new main.
//
// If the watermark is missing or unreadable (first reconcile after
// bootstrap, or transient corruption), we fall back to MergeBase via a
// zero explicit base — the legacy behavior. This is defensive; InitRepo
// and InitFromRemote both seed the watermark, so the missing case
// shouldn't be hit in steady state.
//
// On a successful reconcile, the watermark is advanced to current
// local main. Holds rh.lockBranch(agentBranch) for the duration.
func (rh *repoHandler) reconcileAgent(ctx context.Context, agentBranch string, strategy ConflictStrategy) (ReplayOntoUpstreamResult, error) {
	unlock := rh.lockBranch(agentBranch)
	defer unlock()

	mainRefName := plumbing.NewBranchReferenceName("main")
	mainRef, err := rh.gits.Reference(mainRefName)
	if err != nil {
		return ReplayOntoUpstreamResult{}, fmt.Errorf("reconcileAgent: read local main: %w", err)
	}
	mainHash := mainRef.Hash()

	base, err := rh.readAgentBase(agentBranch)
	if err != nil {
		// Watermark missing — fall back to MergeBase by passing ZeroHash.
		// This handles older repos that predate the watermark and any
		// transient ref corruption. Steady-state init paths seed it.
		log.Warn().
			Str("branch", agentBranch).
			Err(err).
			Msg("reconcileAgent: watermark missing; falling back to MergeBase")
		base = plumbing.ZeroHash
	}

	log.Info().
		Str("branch", agentBranch).
		Str("upstream", "refs/heads/main").
		Str("base", shortRefHash(base)).
		Msg("reconcileAgent: replaying onto local main with watermark base")

	res, err := rh.replayOntoUpstream(ctx, agentBranch, mainHash, base, strategy)
	if err != nil {
		return res, err
	}

	// Advance the watermark to current local main. This is the commit the
	// agent has now "consumed" — the next Sync tick's unpushedCommits walk
	// will use it as its stop point.
	if err := rh.writeAgentBase(agentBranch, mainHash); err != nil {
		return res, fmt.Errorf("reconcileAgent: write watermark: %w", err)
	}
	return res, nil
}

// shortRefHash returns the first 8 chars of a ref hash for log output, or
// "<zero>" for the zero hash.
func shortRefHash(h plumbing.Hash) string {
	if h == plumbing.ZeroHash {
		return "<zero>"
	}
	return h.String()[:8]
}
