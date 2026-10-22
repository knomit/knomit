// Rebase-fallback machinery for the force-rewind case. When local main
// was force-updated to a non-FF target, the merge-based reconcile has
// no anchor (no common ancestor between agent and main). reconcileAgent
// routes here, which walks the agent's local-only commits (since the
// watermark) and replays them onto the new main using replayCommit. The
// watermark stops the walk at the last-consumed main commit, so files
// scrubbed by the rewind don't get resurrected.
package remotesync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	storegit "knomit/internal/store/git"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
	"knomit/internal/store/verify"
)

// unpushedCommits returns commits reachable from localTip but not from
// upstreamTip, ordered oldest → newest (the order in which they should be
// replayed). The walk follows first-parent ancestry and SKIPS merge
// commits.
//
// Merge commits in the agent branch are always reconcile-merges produced
// by reconcileAgentMerge: their first parent is the agent's prior tip,
// their second parent is local main, and their tree includes content
// from the (now-rewound) old main side. Replaying that tree onto the new
// disjoint main would resurrect the scrubbed content the rewind was meant
// to drop. The fix is to skip the merge commit itself but continue the
// first-parent walk past it — agent-only commits made before the merge
// still get replayed, and any agent-only commits made AFTER the merge
// have a first-parent delta that is independent of what the merge pulled
// in from old main (delta is computed against the merge commit's tree).
//
// Not every merge is a reconcile-merge: CommitExperiment's merge brings this
// copy's own experiment commits in through its second parent, and nothing
// else references them once the experiment is deleted. At each merge the walk
// therefore looks behind its non-first parents (sideOf) at the commits held
// by neither the upstream, the stop point's history nor the first-parent
// chain. When every non-merge commit there is signed by this INSTANCE's keys
// (the instance key deps.Signer, shared by all its repositories and branches,
// plus its fleet member record's keys), none changes only replay-skipped
// paths, and the side reaches no held commit off the first parent's history,
// the merge itself is replayed as one commit (its change against its first
// parent), so its own resolution of a conflict comes back as decided. When
// the side also holds anything else, only the instance's own non-merge
// commits are replayed, one by one, ahead of the commits above the merge, and
// a warning names the merge whose own resolution is not replayed. Commits
// signed by anyone else are never replayed.
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
// Whatever the stop point, the walk also stops at the first commit that
// upstreamTip's history holds: a commit already upstream is never replayed.
// "Holds" is upstreamTip's whole history (every parent, not its first-parent
// chain): a member's commits reach a fleet's main through the SECOND parent
// of the master's merge. Stopping there can never skip a local commit:
// ancestry is closed, so everything below a held commit is held too.
//
// Returns empty (and disjoint=false) when localTip is an ancestor of
// upstreamTip (nothing local to replay — caller will fast-forward), OR
// when local is strictly ahead of upstreamTip (strictlyAhead: upstreamTip
// is an ancestor of localTip and the watermark is not beyond it; the
// caller's force-push will fast-forward origin without rewriting commit
// hashes).
func (rh *Repo) unpushedCommits(localTip, upstreamTip, explicitBase plumbing.Hash) ([]*object.Commit, bool, error) {
	commits, disjoint, _, err := rh.unpushedCommitsDropping(localTip, upstreamTip, explicitBase)
	return commits, disjoint, err
}

// unpushedCommitsDropping is unpushedCommits that also reports whether the
// walk dropped a commit that changes only replay-skipped paths
// (SetReplaySkipPaths): the branch must then be rebuilt on the upstream even
// when nothing else is left to replay, or it would carry the dropped commit
// as it stands.
func (rh *Repo) unpushedCommitsDropping(localTip, upstreamTip, explicitBase plumbing.Hash) (collectedOut []*object.Commit, disjointOut, droppedOut bool, errOut error) {
	if localTip == upstreamTip {
		return nil, false, false, nil
	}

	local, err := rh.vr.Repository().CommitObject(localTip)
	if err != nil {
		return nil, false, false, fmt.Errorf("unpushedCommits: local commit %s: %w", localTip, err)
	}
	upstream, err := rh.vr.Repository().CommitObject(upstreamTip)
	if err != nil {
		return nil, false, false, fmt.Errorf("unpushedCommits: upstream commit %s: %w", upstreamTip, err)
	}

	isAncestor, err := local.IsAncestor(upstream)
	if err != nil {
		return nil, false, false, fmt.Errorf("unpushedCommits: IsAncestor (local→upstream): %w", err)
	}
	if isAncestor {
		return nil, false, false, nil
	}

	ahead, err := rh.strictlyAhead(local, upstream, explicitBase)
	if err != nil {
		return nil, false, false, fmt.Errorf("unpushedCommits: %w", err)
	}
	if ahead {
		return nil, false, false, nil
	}

	var stopAt plumbing.Hash
	disjoint := false
	if explicitBase != plumbing.ZeroHash {
		stopAt = explicitBase
	} else {
		bases, err := local.MergeBase(upstream)
		if err != nil {
			return nil, false, false, fmt.Errorf("unpushedCommits: MergeBase: %w", err)
		}
		if len(bases) == 0 {
			disjoint = true
			stopAt = plumbing.ZeroHash
		} else {
			stopAt = bases[0].Hash
		}
	}

	// A commit the upstream already holds is never replayed, whatever the
	// stop point says. The watermark can lie far behind: a copy with no
	// origin (a hub) never advances it, so the first rewind after it attaches
	// one walks back to its InitRepo seed. Replaying a commit that is already
	// upstream re-submits it with the local side winning every conflict, so
	// any later upstream change to a path it wrote is reverted, and the
	// replayed commit is re-signed by THIS copy: the old fleet master's
	// create-time host record and a member's first leaf, after a master
	// switch, refused by the new host's judge on every push (#528 N1).
	var onUpstream map[plumbing.Hash]bool
	if !disjoint {
		onUpstream = map[plumbing.Hash]bool{}
		if err := vcs.WalkHistory(rh.vr.Storer(), upstreamTip, nil, func(c *object.Commit) { onUpstream[c.Hash] = true }); err != nil {
			return nil, false, false, fmt.Errorf("unpushedCommits: upstream history: %w", err)
		}
	}

	// side is what a merge on the walk brought in through its non-first
	// parents that is this copy's own work (ownSideCommits): built on the
	// first merge the walk meets, shared by every merge after it.
	var side *sideWalk

	var collected []*object.Commit
	cur := local
	for {
		if cur.Hash == stopAt || onUpstream[cur.Hash] {
			break
		}
		if cur.NumParents() <= 1 {
			if skip, err := rh.replaySkips(cur); err != nil {
				return nil, false, false, fmt.Errorf("unpushedCommits: %w", err)
			} else if skip {
				droppedOut = true
				log.Info().Str("commit", cur.Hash.String()[:8]).
					Msg("unpushedCommits: dropping a commit that changes only paths a replay never carries")
			} else {
				collected = append(collected, cur)
			}
		} else {
			log.Debug().
				Str("commit", cur.Hash.String()[:8]).
				Int("parents", cur.NumParents()).
				Msg("unpushedCommits: skipping merge commit (reconcile-merge from prior steady-state tick)")
			if side == nil {
				if side, err = rh.newSideWalk(local, stopAt, onUpstream); err != nil {
					return nil, false, false, fmt.Errorf("unpushedCommits: %w", err)
				}
			}
			sr, err := rh.sideOf(side, cur)
			if err != nil {
				return nil, false, false, fmt.Errorf("unpushedCommits: merge %s: %w", cur.Hash.String()[:8], err)
			}
			if sr.dropped {
				droppedOut = true
			}
			switch {
			case len(sr.own) == 0:
				// Nothing of this instance's own behind it: a reconcile
				// merge, or a consensus merge of others' commits. Skipped.
			case sr.foreign == 0 && !sr.dropped && !sr.heldOffLine:
				// Only this instance's own work behind it (an experiment
				// commit, an experiment sync): the merge is replayed as ONE
				// commit, its change against its first parent, so whatever
				// the merge itself decided (a conflict kept on the dst side,
				// a written body, a field merge by the conflicts setting)
				// comes back exactly as decided.
				collected = append(collected, cur)
			default:
				// Mixed: own commits next to others' (foreign, which must
				// never be replayed), a marker-only commit that must be
				// dropped, or commits the upstream or the stop point already
				// holds. The merge's change would carry those, so only the
				// own commits are replayed, one by one: none of the
				// instance's own work is lost, but what the merge itself
				// decided is not replayed, and the log says so.
				log.Warn().Str("merge", cur.Hash.String()[:8]).
					Int("own", len(sr.own)).Int("foreign", sr.foreign).
					Bool("dropped", sr.dropped).Bool("held", sr.heldOffLine).
					Msg("unpushedCommits: replaying a merge's own commits one by one; the merge's own resolution is not replayed")
				// collected is newest first; own is parents first.
				for i := len(sr.own) - 1; i >= 0; i-- {
					collected = append(collected, sr.own[i])
				}
			}
		}
		if cur.NumParents() == 0 {
			break
		}
		parent, err := cur.Parents().Next()
		if err != nil {
			return nil, false, false, fmt.Errorf("unpushedCommits: walk first-parent at %s: %w", cur.Hash, err)
		}
		cur = parent
	}

	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, disjoint, droppedOut, nil
}

// strictlyAhead reports whether local is a strict linear extension of
// upstream that the caller's force-push can send as it stands: upstream is an
// ancestor of local AND the watermark (explicitBase, when set) is upstream
// or one of its ancestors. A watermark beyond upstream means the rewind
// moved main BACK past what this branch consumed (the plainest scrub, a
// reset to an ancestor): what lies between upstream and the watermark is
// what the origin removed, so the branch is not ahead of upstream but
// behind a removal, and the walk must run.
func (rh *Repo) strictlyAhead(local, upstream *object.Commit, explicitBase plumbing.Hash) (bool, error) {
	upstreamAncestor, err := upstream.IsAncestor(local)
	if err != nil {
		return false, fmt.Errorf("IsAncestor (upstream→local): %w", err)
	}
	if !upstreamAncestor || explicitBase == plumbing.ZeroHash || explicitBase == upstream.Hash {
		return upstreamAncestor, nil
	}
	base, err := rh.vr.Repository().CommitObject(explicitBase)
	if err != nil {
		return false, fmt.Errorf("watermark commit %s: %w", explicitBase, err)
	}
	held, err := base.IsAncestor(upstream)
	if err != nil {
		return false, fmt.Errorf("IsAncestor (watermark→upstream): %w", err)
	}
	return held, nil
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
// Note the inversion versus MergeTreesWithStrategy: that helper's
// "Local/Remote" refers to dst/src (merge-target/merge-source), while
// the user-facing replay framing here refers to agent/origin. Inside
// replayCommit, orig is passed as src and ontoCommit as dst, so the
// two namings are opposites. We translate at the boundary so callers
// pass the strategy they actually mean.
func (rh *Repo) replayCommit(
	ctx context.Context,
	orig *object.Commit,
	ontoHash plumbing.Hash,
	strategy merge.ConflictStrategy,
	// extraParents, when given, become the replayed commit's parents after
	// ontoHash: DropUnpublishedCommit replays a merge commit as a merge of
	// the same other parents onto the rebuilt line (orig's delta against its
	// FIRST parent is what the merge brought in).
	extraParents ...plumbing.Hash,
) (plumbing.Hash, error) {
	// The signer is resolved BEFORE the synthetic base or any merged tree is
	// written: a refused replay (ErrNoSigner) must leave no object behind.
	signer, err := rh.vr.CommitSigner()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: %w", err)
	}
	ontoCommit, err := rh.vr.Repository().CommitObject(ontoHash)
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
		// Synthesize an empty-tree commit as the base. MergeTreesWithStrategy
		// only reads .Tree() from baseCommit; an empty tree means every file
		// in orig.TreeHash registers as an Insert (non-conflicting), which is
		// the correct semantic for replaying a root commit.
		//
		// IMPORTANT: object.Commit.Tree() reads through the commit's private
		// storer field; a struct literal would leave that field nil and
		// crash on .Tree(). Storing a synthetic commit and rehydrating via
		// object.GetCommit attaches the storer.
		emptyTreeHash, err := storeEmptyTree(rh.vr.Storer())
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: empty tree: %w", err)
		}
		synth := &object.Commit{
			Author:    object.Signature{Name: "knomit", Email: "knomit@local", When: timeNow()},
			Committer: object.Signature{Name: "knomit", Email: "knomit@local", When: timeNow()},
			Message:   "synthetic empty-tree base",
			TreeHash:  emptyTreeHash,
		}
		enc := rh.vr.Storer().NewEncodedObject()
		if err := synth.Encode(enc); err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: encode synthetic base: %w", err)
		}
		synthHash, err := rh.vr.Storer().SetEncodedObject(enc)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: store synthetic base: %w", err)
		}
		baseCommit, err = object.GetCommit(rh.vr.Storer(), synthHash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: rehydrate synthetic base: %w", err)
		}
	}

	// Strategy translation: in the user-facing replay framing, "Local"
	// refers to this machine's agent (which IS the src commit being
	// replayed). In MergeTreesWithStrategy's framing, "Local" = dst (the
	// side being updated, which is ontoCommit/upstream here). The two
	// "Local"s are OPPOSITES. Invert when calling the merge helper so
	// callers can pass the strategy they actually mean. (See replayCommit
	// doc comment.)
	var mergeStrategy merge.ConflictStrategy
	var resolutions map[string]merge.Resolution
	var trailers []string
	policy, settle := merge.ConflictsPolicyOf(strategy)
	switch {
	case settle:
		// The `conflicts` setting, with THIS framing's sides: the consensus
		// side is ontoCommit (dst), and a key set to off keeps today's replay
		// rule, the agent's commit (src) wins. Its lines go on the replayed
		// commit's own message — a replay writes no merge commit.
		res, lines, err := rh.mr.FactMergeResolutions(ctx, baseCommit, orig, ontoCommit,
			merge.FactMerge{Policy: policy, Consensus: fact.MergeDst, Fallback: merge.StrategyRemoteWins})
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("replayCommit: merge facts: %w", err)
		}
		mergeStrategy, resolutions, trailers = merge.StrategyRefuse, res, lines
	case strategy == merge.StrategyLocalWins:
		mergeStrategy = merge.StrategyRemoteWins
	case strategy == merge.StrategyRemoteWins:
		mergeStrategy = merge.StrategyLocalWins
	default:
		// Empty / unrecognized: default to agent-wins (the project decision
		// for origin sync replay).
		mergeStrategy = merge.StrategyRemoteWins
	}

	// Three-way merge: base = baseCommit (orig's parent or empty),
	//                  src  = orig (what orig adds),
	//                  dst  = ontoCommit (what we're replaying on top of).
	mergedTreeHash, walkTrailers, err := rh.mr.MergeTreesWithStrategy(ctx, baseCommit, orig, ontoCommit, mergeStrategy, resolutions)
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
		Message:      merge.AppendTrailerLines(orig.Message, append(trailers, walkTrailers...)),
		TreeHash:     mergedTreeHash,
		ParentHashes: append([]plumbing.Hash{ontoHash}, extraParents...),
	}

	h, err := vcs.StoreCommit(rh.vr.Storer(), signer, newCommit)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("replayCommit: %w", err)
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
// Caller must hold rh.LockBranch(localBranch).
func (rh *Repo) replayOntoUpstream(
	ctx context.Context,
	localBranch string,
	upstreamTip plumbing.Hash,
	explicitBase plumbing.Hash,
	strategy merge.ConflictStrategy,
) (merge.AgentReconcileResult, error) {
	localRefName := plumbing.NewBranchReferenceName(localBranch)
	localRef, err := rh.vr.Storer().Reference(localRefName)
	if err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: local ref %q: %w", localBranch, err)
	}
	localTip := localRef.Hash()

	commits, disjoint, dropped, err := rh.unpushedCommitsDropping(localTip, upstreamTip, explicitBase)
	if err != nil {
		return merge.AgentReconcileResult{}, err
	}

	// No unpushed commits. Two ancestry shapes keep the branch's hashes:
	//   1. local is an ancestor of upstream → fast-forward local to upstream.
	//   2. local is strictly ahead of upstream (strictlyAhead: upstream is an
	//      ancestor of local AND the watermark is at or below upstream); the
	//      caller's force-push will fast-forward origin. Do NOT rewind local
	//      — that would orphan its branch_commits rows.
	//   3. local and upstream are disjoint (no common history): kept as it
	//      stands, as before (see the comment at the check).
	// Every other empty walk is a branch that diverged from upstream with
	// nothing of this instance's own beyond the watermark: what it holds
	// beyond upstream is what the rewind removed (a scrubbed commit at the
	// watermark, old main, merges with nothing of the instance's own behind
	// them). Kept as it stands, the caller's force-push would send that back
	// to the origin that removed it, so it is rebuilt on upstream with
	// nothing to replay, as is a walk that DROPPED a commit
	// (SetReplaySkipPaths). An own experiment merge is not lost by this: the
	// walk collects it (sideOf), so its walk is not empty.
	if len(commits) == 0 && !dropped {
		if localTip == upstreamTip {
			return merge.AgentReconcileResult{Mode: merge.ModeNoop}, nil
		}
		localCommit, err := rh.vr.Repository().CommitObject(localTip)
		if err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: local commit: %w", err)
		}
		upstreamCommit, err := rh.vr.Repository().CommitObject(upstreamTip)
		if err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: upstream commit: %w", err)
		}
		isLocalAncestor, err := localCommit.IsAncestor(upstreamCommit)
		if err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: IsAncestor: %w", err)
		}
		if isLocalAncestor {
			if err := rh.vr.DeriveBeforeAdvance(ctx, upstreamTip); err != nil {
				return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: fast-forward: %w", err)
			}
			newRef := plumbing.NewHashReference(localRefName, upstreamTip)
			if err := rh.vr.Storer().SetReference(newRef); err != nil {
				return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: fast-forward: %w", err)
			}
			if err := rh.vr.PopulateCommitLog(ctx, localBranch); err != nil {
				return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: populate after FF: %w", err)
			}
			if err := rh.vr.NotifyCommit(ctx, localBranch, upstreamTip); err != nil {
				return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: notify after FF: %w", err)
			}
			return merge.AgentReconcileResult{Mode: merge.ModeFF, NewTip: upstreamTip.String()}, nil
		}
		ahead, err := rh.strictlyAhead(localCommit, upstreamCommit, explicitBase)
		if err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: %w", err)
		}
		if ahead {
			// No replay, no fast-forward: the caller's force-push will
			// advance origin to local.
			return merge.AgentReconcileResult{Mode: merge.ModeNoop, NewTip: localTip.String()}, nil
		}
		bases, err := localCommit.MergeBase(upstreamCommit)
		if err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: MergeBase: %w", err)
		}
		if len(bases) == 0 {
			// Disjoint histories keep the no-op: attaching a fresh local
			// repository to a remote of the same knowledge base is this
			// shape too (its watermark is the InitRepo seed, never on any
			// origin), and taking the remote's main there would change the
			// repository's identity (its root commit). A history rewritten
			// down to its root to scrub something is the same shape; that
			// residual is not closed here.
			log.Info().Str("branch", localBranch).Str("upstream", upstreamTip.String()[:8]).
				Msg("replayOntoUpstream: nothing to replay onto a disjoint upstream; the branch stays as it is")
			return merge.AgentReconcileResult{Mode: merge.ModeNoop, NewTip: localTip.String()}, nil
		}
		log.Info().Str("branch", localBranch).Str("upstream", upstreamTip.String()[:8]).
			Msg("replayOntoUpstream: nothing of the branch's own to replay; rebuilding it on upstream so what the rewind removed is not pushed back")
	}

	log.Info().
		Str("branch", localBranch).
		Int("count", len(commits)).
		Bool("disjoint", disjoint).
		Str("upstream", upstreamTip.String()[:8]).
		Msg("replayOntoUpstream: starting")

	tempRefName := plumbing.NewBranchReferenceName(localBranch + "-replaying")
	// Start temp ref at upstream tip.
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(tempRefName, upstreamTip)); err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: init temp ref: %w", err)
	}
	// Always remove the temp ref before returning — success or failure —
	// so the branch namespace doesn't accumulate stale "-replaying" refs.
	// The check via Reference() guards against double-removal noise if a
	// future code path drops the ref explicitly before returning.
	defer func() {
		if _, err := rh.vr.Storer().Reference(tempRefName); err == nil {
			if rmErr := rh.vr.Storer().RemoveReference(tempRefName); rmErr != nil {
				log.Warn().Err(rmErr).Msg("replayOntoUpstream: remove temp ref (continuing)")
			}
		}
	}()

	current := upstreamTip
	for i, orig := range commits {
		// Honour cancellation between commits: a large rewind under server
		// shutdown (or HTTP client disconnect) would otherwise hold
		// LockBranch(localBranch) until the full chain finishes. Local
		// branch is unchanged at this point (only the temp ref has
		// advanced), and the deferred temp-ref cleanup runs on return.
		if err := ctx.Err(); err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: cancelled at step %d/%d: %w",
				i+1, len(commits), err)
		}
		newHash, err := rh.replayCommit(ctx, orig, current, strategy)
		if err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: replay step %d/%d (%s): %w",
				i+1, len(commits), orig.Hash.String()[:8], err)
		}
		current = newHash
		// Advance temp ref so each replayed commit is reachable.
		if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(tempRefName, current)); err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: advance temp ref: %w", err)
		}
	}

	// Atomic move: agent ref → new tip. Temp ref is cleaned up by the
	// deferred removal above.
	if err := rh.vr.DeriveBeforeAdvance(ctx, current); err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: %w", err)
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(localRefName, current)); err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: atomic move: %w", err)
	}

	// The pre-replay chain is no longer reachable from localBranch. Purge
	// stale branch_commits rows and rebuild parity from the new tip — in one
	// transaction, so no reader sees the branch empty in between — without
	// Verify complaining about unreachable rows.
	if err := rh.vr.RepopulateBranch(ctx, localBranch); err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: repopulate: %w", err)
	}
	if err := rh.vr.NotifyCommit(ctx, localBranch, current); err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("replayOntoUpstream: notify: %w", err)
	}

	log.Info().
		Str("branch", localBranch).
		Int("count", len(commits)).
		Str("new_tip", current.String()[:8]).
		Msg("replayOntoUpstream: complete")

	return merge.AgentReconcileResult{
		Mode:        merge.ModeRebase,
		NumReplayed: len(commits),
		NewTip:      current.String(),
	}, nil
}

// reconcileAgentRebase is the rewind-fallback reconcile path. Reads the
// watermark, walks the agent's local-only commits, and replays them onto
// current local main via replayOntoUpstream. Only invoked from
// reconcileAgent when reconcileMain reported Mode=ModeRewound.
//
// Falls back to MergeBase (watermark=zero) when the watermark has never
// been written (plumbing.ErrReferenceNotFound). Sync seeds a missing
// watermark from where the origin was at the last fetch first
// (seedMissingWatermark), so this is reached only on a truly first sync, or
// from a caller that does not fetch. Any OTHER error reading the watermark
// is surfaced rather than silently downgraded to a full-history walk.
//
// Merge commits from prior steady-state ticks are skipped by
// unpushedCommits' walk (see that function's doc), so a force-rewind
// following an established sync history does not resurrect old-main
// content via the merge commit's tree.
//
// On a successful reconcile, the watermark is advanced to current local
// main. Holds rh.LockBranch(agentBranch) for the duration.
func (rh *Repo) reconcileAgentRebase(ctx context.Context, agentBranch, upstreamMain string, strategy merge.ConflictStrategy) (merge.AgentReconcileResult, error) {
	unlock := rh.vr.LockBranch(agentBranch)
	defer unlock()

	mainRefName := plumbing.NewBranchReferenceName(upstreamMain)
	mainRef, err := rh.vr.Storer().Reference(mainRefName)
	if err != nil {
		return merge.AgentReconcileResult{}, fmt.Errorf("reconcileAgentRebase: read local %s: %w", upstreamMain, err)
	}
	mainHash := mainRef.Hash()

	base, err := rh.readAgentBase(agentBranch)
	if err != nil {
		if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return merge.AgentReconcileResult{}, fmt.Errorf("reconcileAgentRebase: read watermark: %w", err)
		}
		log.Warn().
			Str("branch", agentBranch).
			Msg("reconcileAgentRebase: watermark missing; falling back to MergeBase")
		base = plumbing.ZeroHash
	}

	log.Info().
		Str("branch", agentBranch).
		Str("upstream", "refs/heads/"+upstreamMain).
		Str("base", vcs.ShortRefHash(base)).
		Msgf("reconcileAgentRebase: replaying onto local %s with watermark base", upstreamMain)

	// Every experiment of this branch is recorded BEFORE the branch moves:
	// experiments are not rebuilt here, and one that still holds what this
	// rewind scrubs must be rebuilt by its next commit or sync, not merged
	// (rebuildRewoundExperiment).
	if agentRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(agentBranch)); err == nil {
		if err := rh.mr.MarkExperimentsRewound(ctx, agentBranch, agentRef.Hash()); err != nil {
			return merge.AgentReconcileResult{}, fmt.Errorf("reconcileAgentRebase: %w", err)
		}
	}

	res, err := rh.replayOntoUpstream(ctx, agentBranch, mainHash, base, strategy)
	if err != nil {
		return res, err
	}

	if err := rh.WriteAgentBase(agentBranch, mainHash); err != nil {
		return res, fmt.Errorf("reconcileAgentRebase: write watermark: %w", err)
	}
	return res, nil
}

// replaySkips reports whether c changes only replay-skipped paths (and at
// least one).
func (rh *Repo) replaySkips(c *object.Commit) (bool, error) {
	if len(rh.vr.ReplaySkip()) == 0 || c.NumParents() != 1 {
		return false, nil
	}
	parent, err := c.Parents().Next()
	if err != nil {
		return false, err
	}
	pt, err := parent.Tree()
	if err != nil {
		return false, err
	}
	ct, err := c.Tree()
	if err != nil {
		return false, err
	}
	changes, err := object.DiffTree(pt, ct)
	if err != nil {
		return false, err
	}
	if len(changes) == 0 {
		return false, nil
	}
	for _, ch := range changes {
		for _, name := range []string{ch.From.Name, ch.To.Name} {
			if name != "" && !rh.vr.ReplaySkip()[name] {
				return false, nil
			}
		}
	}
	return true, nil
}

// sideWalk is the state sideOf shares across the merges of one
// unpushedCommits walk: this instance's key fingerprints, the commits the
// upstream or the stop point already hold, and every commit a side walk must
// not enter or has already entered.
type sideWalk struct {
	own  map[string]bool
	held map[plumbing.Hash]bool
	stop map[plumbing.Hash]bool
}

// newSideWalk builds the side walk's state for a first-parent walk from local
// that stops at stopAt or at a commit onUpstream holds. The own keys are
// checkOwnLineage's (E4): the key this store signs with, which is the
// INSTANCE key (deps.Signer, rh.signer), shared by every repository and
// branch on this instance, plus every key this instance's fleet member record
// has held. "Own" therefore means "this instance's", not "this agent
// branch's". held is the upstream's history and the stop point's history;
// stop is held plus the first-parent chain the main walk visits itself.
func (rh *Repo) newSideWalk(local *object.Commit, stopAt plumbing.Hash, onUpstream map[plumbing.Hash]bool) (*sideWalk, error) {
	sw := &sideWalk{own: map[string]bool{}, held: map[plumbing.Hash]bool{}, stop: map[plumbing.Hash]bool{}}
	if signer, err := rh.vr.CommitSigner(); err == nil {
		if fp, err := verify.KeyFingerprint(signer.PublicKey()); err == nil {
			sw.own[fp] = true
		}
	}
	for _, k := range rh.vr.OwnKeys() {
		if fp, err := verify.KeyFingerprint(k); err == nil {
			sw.own[fp] = true
		}
	}
	if len(sw.own) == 0 {
		return sw, nil
	}
	for h := range onUpstream {
		sw.held[h] = true
	}
	if stopAt != plumbing.ZeroHash {
		// onUpstream is ancestry-closed, so it prunes this walk.
		if err := vcs.WalkHistory(rh.vr.Storer(), stopAt, sw.held, func(c *object.Commit) { sw.held[c.Hash] = true }); err != nil {
			return nil, fmt.Errorf("stop point history: %w", err)
		}
		sw.held[stopAt] = true
	}
	for h := range sw.held {
		sw.stop[h] = true
	}
	for c := local; ; {
		if c.Hash == stopAt || onUpstream[c.Hash] {
			break
		}
		sw.stop[c.Hash] = true
		if c.NumParents() == 0 {
			break
		}
		p, err := c.Parents().Next()
		if err != nil {
			return nil, fmt.Errorf("first-parent chain at %s: %w", c.Hash, err)
		}
		c = p
	}
	return sw, nil
}

// sideResult is what sideOf found behind one merge's non-first parents.
type sideResult struct {
	// own: the non-merge commits signed by this instance's keys, parents
	// first, less those that change only replay-skipped paths.
	own []*object.Commit
	// foreign: how many non-merge commits are signed by any other key, or
	// unsigned.
	foreign int
	// dropped: an own commit changed only replay-skipped paths.
	dropped bool
	// heldOffLine: the side reaches a commit the upstream or the stop point
	// holds that is NOT an ancestor of the merge's first parent, so the
	// merge's change against its first parent carries content already held.
	heldOffLine bool
}

// sideOf walks what merge brought in through its NON-FIRST parents: every
// commit not held by the upstream, the stop point's history, the
// first-parent chain, or an earlier merge's side walk. Its non-merge commits
// are sorted into this instance's own (sideWalk.own) and everyone else's.
//
// A committed experiment's work lives there: CommitExperiment writes a merge
// whose second parent is the experiment and deletes the experiment, so the
// merge is the only reference to that work, and the first-parent walk, which
// skips merges, would lose it on the first rewind after any later write. So
// does the agent's own work an experiment's sync merge brought in, when the
// experiment commit then fast-forwarded the agent branch onto the
// experiment. A commit signed by anyone else (a member's commit in a hub
// master's consensus merge) is never replayed: it would be re-signed as this
// instance, which a fleet's new host refuses for good (#528 N1).
func (rh *Repo) sideOf(sw *sideWalk, merge *object.Commit) (sideResult, error) {
	var res sideResult
	if len(sw.own) == 0 {
		return res, nil
	}
	boundary := map[plumbing.Hash]bool{}
	var visited []*object.Commit
	type frame struct {
		c    *object.Commit
		next int
	}
	enter := func(h plumbing.Hash) (*frame, error) {
		if sw.held[h] {
			boundary[h] = true
			return nil, nil
		}
		if sw.stop[h] {
			return nil, nil
		}
		sw.stop[h] = true
		c, err := object.GetCommit(rh.vr.Storer(), h)
		if err != nil {
			return nil, err
		}
		return &frame{c: c}, nil
	}
	// A depth-first post-order walk (parents before children), as
	// WalkHistory, that also records where it met a held commit.
	for _, p := range merge.ParentHashes[1:] {
		root, err := enter(p)
		if err != nil {
			return res, err
		}
		if root == nil {
			continue
		}
		stack := []*frame{root}
		for len(stack) > 0 {
			f := stack[len(stack)-1]
			if f.next < len(f.c.ParentHashes) {
				pf, err := enter(f.c.ParentHashes[f.next])
				f.next++
				if err != nil {
					return res, err
				}
				if pf != nil {
					stack = append(stack, pf)
				}
				continue
			}
			stack = stack[:len(stack)-1]
			visited = append(visited, f.c)
		}
	}
	for _, c := range visited {
		if c.NumParents() > 1 {
			continue
		}
		s, serr := verify.VerifyCommitSignature(c)
		if serr != nil || !sw.own[s.Fingerprint] {
			res.foreign++
			continue
		}
		skip, err := rh.replaySkips(c)
		if err != nil {
			return res, err
		}
		if skip {
			res.dropped = true
			log.Info().Str("commit", c.Hash.String()[:8]).
				Msg("unpushedCommits: dropping a merged-in commit that changes only paths a replay never carries")
			continue
		}
		res.own = append(res.own, c)
	}
	// Only a side that would be replayed as the merge itself needs the
	// held-commit check: a held commit the first parent does not reach is
	// content the merge's change carries although it is already held.
	if len(res.own) > 0 && res.foreign == 0 && !res.dropped && len(boundary) > 0 {
		first, err := merge.Parents().Next()
		if err != nil {
			return res, err
		}
		for h := range boundary {
			if h == first.Hash {
				continue
			}
			hc, err := object.GetCommit(rh.vr.Storer(), h)
			if err != nil {
				return res, err
			}
			ok, err := hc.IsAncestor(first)
			if err != nil {
				return res, err
			}
			if !ok {
				res.heldOffLine = true
				break
			}
		}
	}
	return res, nil
}
