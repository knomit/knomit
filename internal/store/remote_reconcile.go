// Origin reconciliation primitives. The agent branch is reconciled by
// replaying its unpushed commits onto an upstream (origin/agent/<host>
// when present, else origin/main); local main is a strict mirror of
// origin/main. The reconcile primitives are the single source of truth
// for "what does sync do" — Sync/Push/InitFromRemote/ActivateSync all
// call into them.
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

// agentUpstream identifies the ref the agent branch should reconcile against.
type agentUpstream struct {
	refName plumbing.ReferenceName // remote-tracking ref name (refs/remotes/origin/...)
	hash    plumbing.Hash          // current tip of that ref
	// isOwnAgent is true when the upstream is origin/agent/<host>.
	// false when we fell back to origin/main (no remote agent branch yet).
	isOwnAgent bool
}

// resolveAgentUpstream returns the upstream the local agent branch should
// reconcile against: origin/agent/<host> when that ref exists locally
// (post-fetch), otherwise origin/main. Errors if neither ref exists.
func (rh *repoHandler) resolveAgentUpstream(ctx context.Context, agentBranch string) (agentUpstream, error) {
	agentRefName := plumbing.NewRemoteReferenceName("origin", agentBranch)
	if ref, err := rh.gits.Reference(agentRefName); err == nil {
		return agentUpstream{refName: agentRefName, hash: ref.Hash(), isOwnAgent: true}, nil
	}
	mainRefName := plumbing.NewRemoteReferenceName("origin", "main")
	if ref, err := rh.gits.Reference(mainRefName); err == nil {
		return agentUpstream{refName: mainRefName, hash: ref.Hash(), isOwnAgent: false}, nil
	}
	return agentUpstream{}, fmt.Errorf("resolveAgentUpstream: neither origin/%s nor origin/main present (fetch first?)", agentBranch)
}

// unpushedCommits returns commits reachable from localTip but not from
// upstreamTip, ordered oldest → newest (the order in which they should be
// replayed). The walk follows first-parent ancestry only — merge commits
// take their "ours" side, matching the linear-history goal.
//
// Returns disjoint=true when localTip and upstreamTip share no common
// ancestor: every local commit (back to root) is treated as unpushed.
// In that case the caller will replay the entire chain onto upstreamTip.
//
// Returns empty (and disjoint=false) when localTip is an ancestor of
// upstreamTip (nothing local to replay — caller will fast-forward).
func (rh *repoHandler) unpushedCommits(localTip, upstreamTip plumbing.Hash) ([]*object.Commit, bool, error) {
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
		return nil, false, fmt.Errorf("unpushedCommits: IsAncestor: %w", err)
	}
	if isAncestor {
		return nil, false, nil
	}

	bases, err := local.MergeBase(upstream)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: MergeBase: %w", err)
	}

	var stopAt plumbing.Hash
	disjoint := false
	if len(bases) == 0 {
		disjoint = true
		// Walk all the way back to root.
		stopAt = plumbing.ZeroHash
	} else {
		stopAt = bases[0].Hash
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
		emptyTreeHash, err := storeEmptyTree(rh.gits)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: empty tree: %w", err)
		}
		baseCommit = &object.Commit{TreeHash: emptyTreeHash}
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
	strategy ConflictStrategy,
) (ReplayOntoUpstreamResult, error) {
	localRefName := plumbing.NewBranchReferenceName(localBranch)
	localRef, err := rh.gits.Reference(localRefName)
	if err != nil {
		return ReplayOntoUpstreamResult{}, fmt.Errorf("replayOntoUpstream: local ref %q: %w", localBranch, err)
	}
	localTip := localRef.Hash()

	commits, disjoint, err := rh.unpushedCommits(localTip, upstreamTip)
	if err != nil {
		return ReplayOntoUpstreamResult{}, err
	}

	// No unpushed commits.
	if len(commits) == 0 {
		// Local may still be behind upstream — fast-forward if so.
		if localTip != upstreamTip {
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
		return ReplayOntoUpstreamResult{}, nil
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
		log.Info().Str("to", originHash.String()[:8]).Msg("reconcileMain: fast-forward")
		return MainReconcileResult{FastForward: true, NewTip: originHash.String()}, nil
	}

	// origin/main is not a descendant of local main → rewind / divergent advance.
	// Force-update local main; caller is responsible for re-migrating the agent.
	if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: force-update: %w", err)
	}
	log.Warn().
		Str("local", localHash.String()[:8]).
		Str("origin", originHash.String()[:8]).
		Msg("reconcileMain: origin/main is not a descendant of local main; force-updated")
	return MainReconcileResult{Rewound: true, NewTip: originHash.String()}, nil
}
