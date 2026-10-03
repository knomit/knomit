// Real git merge for MergeBranch and the strategy-aware three-way tree merge
// helper shared with remoteIndex.Sync. Lives on repoHandler because all the
// inputs (git storer, repo, signer, branch lock, commit-log plumbing) are
// rooted there.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// MergeBranch merges src into dst using the given conflict strategy.
// Thin wrapper around mergeIntoBranch that discards the structured result;
// preserves the public BranchIndex interface for existing callers.
func (rh *repoHandler) MergeBranch(ctx context.Context, src, dst string, strategy ConflictStrategy) error {
	_, err := rh.mergeIntoBranch(ctx, src, dst, strategy)
	return err
}

// mergeIntoBranch acquires rh.lockBranch(dst) and calls
// mergeIntoBranchLocked. Callers that already hold the lock (e.g.
// reconcileAgentMerge, which holds it for the watermark write) should
// call mergeIntoBranchLocked directly.
func (rh *repoHandler) mergeIntoBranch(
	ctx context.Context,
	src, dst string,
	strategy ConflictStrategy,
) (AgentReconcileResult, error) {
	return rh.mergeIntoBranchResolved(ctx, src, dst, strategy, nil)
}

// mergeIntoBranchResolved is mergeIntoBranch with adjudications for paths that
// would otherwise be refused. Everything else merges exactly as before: a
// resolution is per-path and says nothing about any other path.
//
// It takes the DST lock, like mergeIntoBranch, and carries it through
// notifyCommit (invariants/store/branch-lock-spans-notify).
func (rh *repoHandler) mergeIntoBranchResolved(
	ctx context.Context,
	src, dst string,
	strategy ConflictStrategy,
	resolutions map[string]Resolution,
) (AgentReconcileResult, error) {
	unlock := rh.lockBranch(dst)
	defer unlock()
	return rh.mergeIntoBranchLockedResolved(ctx, src, dst, strategy, resolutions)
}

// mergeIntoBranchLocked merges src into dst using the given conflict strategy
// and returns a structured AgentReconcileResult describing what happened.
// Caller must hold rh.lockBranch(dst).
//
// Modes:
//   - ModeNoop:  src is ancestor of dst (or hashes match); dst unchanged.
//   - ModeFF:    dst is ancestor of src; dst fast-forwarded to src.
//   - ModeMerge: divergent histories; one merge commit synthesized whose
//     first parent is the previous dst tip and second parent is
//     src. The merged tree is produced by mergeTreesWithStrategy
//     with the given conflict strategy.
//
// When the three-way merge produces a tree identical to dst's tree (every
// src change was either no-op or skipped by strategy), the result is
// reported as ModeNoop rather than synthesizing a zero-diff merge commit, so
// a sync that changes nothing leaves no trace. (It is not needed for
// commit-log parity: checkCommitLogParity compares against branch_commits
// visibility and accepts no-op commits. The peer merge's `record` option
// writes such a commit on purpose — mergeOpts.)
//
// Errors if dst/src refs cannot be resolved or if histories are disjoint
// (no common ancestor — the caller is responsible for routing to the
// rebase fallback in that case).
func (rh *repoHandler) mergeIntoBranchLocked(
	ctx context.Context,
	src, dst string,
	strategy ConflictStrategy,
) (AgentReconcileResult, error) {
	return rh.mergeIntoBranchLockedResolved(ctx, src, dst, strategy, nil)
}

func (rh *repoHandler) mergeIntoBranchLockedResolved(
	ctx context.Context,
	src, dst string,
	strategy ConflictStrategy,
	resolutions map[string]Resolution,
) (AgentReconcileResult, error) {
	return rh.mergeIntoBranchLockedOpts(ctx, src, dst, strategy, resolutions, mergeOpts{})
}

// mergeOpts are the peer-merge caller's options (F11 UI merge). The zero value
// is the merge every other caller has always had.
type mergeOpts struct {
	// srcTip, when set, is the src commit the caller confirmed: the merge
	// refuses with *BranchMovedError if src no longer points there, and
	// otherwise merges exactly that commit.
	srcTip plumbing.Hash
	// record always writes a merge commit [dst, src]: never a fast-forward and
	// never a tree-identical no-op. Only "src is already in dst" is a no-op.
	record bool
	// skipMergeOnly (with record) adds ONE no-op: the merge would leave
	// dst's tree unchanged AND every commit src brings is itself a merge
	// commit — src only merged dst's own history back in. The consensus
	// merger needs it: a peer whose sync always writes a merge commit (never
	// fast-forwards) would otherwise get a new host merge commit to merge
	// back on every round, forever.
	skipMergeOnly bool
	// side, when set, settles EVERY conflicting path with that side, detected
	// under the dst lock (the whole-set choice of the UI merge dialog).
	side ResolutionSide
	// factConsensus and factFallback say how a `conflicts` strategy runs at
	// this site: which side is the consensus side, and the side-picking
	// strategy a key set to off gets. The zero values are the peer sync's:
	// the consensus branch is src (merged INTO the agent branch), and an off
	// key is LocalWins, as always.
	factConsensus fact.MergeSide
	factFallback  ConflictStrategy
	// trace, when set, is stamped on the merge commit (joined to its trailer
	// paragraph). ONLY an experiment's own merges set it — CommitExperiment
	// and SyncExperiment, from the agent `trace` of the knomit_experiment
	// call — so every other merge (agent sync, reconcile, the peer merge)
	// stays a transport commit with no trace, as before. A fast-forward or a
	// no-op writes no commit and so stamps nothing.
	trace Trailers
	// overlayResolutions lets a `conflicts` strategy take the caller's
	// resolutions as well: each path the caller named takes the caller's
	// resolution, and the setting settles every other conflicting path. ONLY
	// CommitExperiment sets it. Without it a `conflicts` strategy refuses any
	// caller resolution, which is what every other site keeps.
	//
	// Without the overlay the setting would stop applying the moment an agent
	// followed a refusal's instructions: the refusal lists only what the
	// setting left, so a retry naming just those paths would turn the setting
	// off and be refused again for paths it had already settled.
	overlayResolutions bool
}

func (o mergeOpts) factMerge(p conflictsPolicy) factMerge {
	fm := factMerge{policy: p, consensus: o.factConsensus, fallback: o.factFallback}
	if fm.consensus == "" {
		fm.consensus = fact.MergeSrc
	}
	if fm.fallback == "" {
		fm.fallback = StrategyLocalWins
	}
	return fm
}

func (rh *repoHandler) mergeIntoBranchLockedOpts(
	ctx context.Context,
	src, dst string,
	strategy ConflictStrategy,
	resolutions map[string]Resolution,
	o mergeOpts,
) (AgentReconcileResult, error) {
	if strategy == "" {
		strategy = StrategyLocalWins
	}

	if _, err := rh.branchID(ctx, src); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: src %q: %w", src, err)
	}
	if _, err := rh.branchID(ctx, dst); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: dst %q: %w", dst, err)
	}

	srcRefName := plumbing.NewBranchReferenceName(src)
	dstRefName := plumbing.NewBranchReferenceName(dst)

	srcRef, err := rh.gits.Reference(srcRefName)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: resolve src ref %q: %w", src, err)
	}
	dstRef, err := rh.gits.Reference(dstRefName)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: resolve dst ref %q: %w", dst, err)
	}
	srcHash := srcRef.Hash()
	dstHash := dstRef.Hash()
	if !o.srcTip.IsZero() && srcHash != o.srcTip {
		return AgentReconcileResult{}, &BranchMovedError{Branch: src, Expected: o.srcTip.String(), Actual: srcHash.String()}
	}

	if srcHash == dstHash {
		// Nothing to merge means nothing conflicted, so every resolution the
		// caller sent adjudicated nothing. This return never reaches the tree
		// walk, which is why the check is repeated here rather than left to it.
		if err := noLeftoverResolutions(resolutions, nil); err != nil {
			return AgentReconcileResult{}, err
		}
		return AgentReconcileResult{Mode: ModeNoop, NewTip: dstHash.String()}, nil
	}

	srcCommit, err := rh.repo.CommitObject(srcHash)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: src commit: %w", err)
	}
	dstCommit, err := rh.repo.CommitObject(dstHash)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: dst commit: %w", err)
	}

	isSrcAncestor, err := srcCommit.IsAncestor(dstCommit)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: check src ancestor: %w", err)
	}
	if isSrcAncestor {
		// src is already contained in dst: nothing to merge, so nothing
		// conflicted and any resolution adjudicated nothing.
		if err := noLeftoverResolutions(resolutions, nil); err != nil {
			return AgentReconcileResult{}, err
		}
		return AgentReconcileResult{Mode: ModeNoop, NewTip: dstHash.String()}, nil
	}

	isDstAncestor, err := dstCommit.IsAncestor(srcCommit)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: check dst ancestor: %w", err)
	}
	if isDstAncestor && !o.record {
		// A fast-forward takes src wholesale: nothing conflicted, so a
		// resolution here adjudicated nothing. Checked BEFORE the ref moves —
		// a rejected call must leave the branch exactly where it was.
		if err := noLeftoverResolutions(resolutions, nil); err != nil {
			return AgentReconcileResult{}, err
		}
		if err := rh.deriveBeforeAdvance(ctx, srcHash); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: %w", err)
		}
		newRef := plumbing.NewHashReference(dstRefName, srcHash)
		if err := rh.gits.SetReference(newRef); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: fast-forward ref: %w", err)
		}
		if err := rh.populateCommitLog(ctx, dst); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: fast-forward populate: %w", err)
		}
		if err := rh.notifyCommit(ctx, dst, srcHash); err != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: fast-forward notify: %w", err)
		}
		log.Info().
			Str("src", src).Str("dst", dst).
			Str("to", srcHash.String()[:8]).
			Msg("mergeIntoBranch: fast-forward")
		return AgentReconcileResult{Mode: ModeFF, NewTip: srcHash.String()}, nil
	}

	bases, err := dstCommit.MergeBase(srcCommit)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: merge base: %w", err)
	}
	if len(bases) == 0 {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: no common ancestor between %q and %q (disjoint histories): %w", src, dst, ErrUnrelatedHistories)
	}
	baseCommit := bases[0]

	policy, mergeFacts := conflictsPolicyOf(strategy)
	if mergeFacts && ((len(resolutions) > 0 && !o.overlayResolutions) || o.side != "") {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: %s computes its own resolutions; none may be given", strategy)
	}

	// trailers is the merge commit's record of every conflict it settles.
	var trailers []string
	if o.side != "" && len(resolutions) == 0 {
		detected, derr := rh.detectConflicts(ctx, baseCommit, srcCommit, dstCommit)
		if derr != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: detect conflicts: %w", derr)
		}
		resolutions = make(map[string]Resolution, len(detected))
		for p := range detected {
			resolutions[p] = Resolution{Side: o.side}
		}
		if trailers, derr = sideResolutionLines(baseCommit, srcCommit, dstCommit, detected, o.side); derr != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: record side: %w", derr)
		}
	}

	// EVERY resolution is checked against the real conflict set BEFORE the
	// merge that consumes them. Doing it inside the walk cannot be complete:
	// a path only DST changed never appears in DiffTree(base, src), so the
	// walk never sees it and a resolution naming it would be silently
	// dropped — leaving the caller believing it settled something it did not.
	if len(resolutions) > 0 {
		detected, derr := rh.detectConflicts(ctx, baseCommit, srcCommit, dstCommit)
		if derr != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: detect conflicts: %w", derr)
		}
		if err := noLeftoverResolutions(resolutions, detected); err != nil {
			return AgentReconcileResult{}, err
		}
	}

	// The signer is resolved BEFORE the three-way merge writes any tree: a
	// refused merge (ErrNoSigner) must leave no object behind.
	signer, err := rh.commitSigner()
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: %w", err)
	}
	// The merge commit's author is resolved here too, for the same reason: an
	// experiment whose owning agent cannot be named (commitAgentID) refuses
	// the merge before any tree is written.
	author, committer, err := rh.commitSigs(ctx, dst, "merge")
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: %w", err)
	}

	// A `conflicts` strategy settles the conflict set up front, as per-path
	// resolutions the REFUSING walk applies; whatever it could not settle
	// (its site falls back to Refuse) is refused exactly as before.
	walk := strategy
	if mergeFacts {
		res, lines, ferr := rh.factMergeResolutions(ctx, baseCommit, srcCommit, dstCommit, o.factMerge(policy))
		if ferr != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: merge facts: %w", ferr)
		}
		if o.overlayResolutions && len(resolutions) > 0 {
			// The caller's map was already checked against the real conflict
			// set above (noLeftoverResolutions), so every entry names a path
			// the setting just decided; the caller's decision replaces it.
			if res, lines, ferr = overlayCallerResolutions(res, lines, resolutions, baseCommit, srcCommit, dstCommit); ferr != nil {
				return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: overlay resolutions: %w", ferr)
			}
		}
		resolutions, trailers, walk = res, lines, StrategyRefuse
	}

	mergedTreeHash, walkTrailers, err := rh.mergeTreesWithStrategy(ctx, baseCommit, srcCommit, dstCommit, walk, resolutions)
	trailers = append(trailers, walkTrailers...)
	settled := settledFrom(trailers)
	if err != nil {
		// A refusal is not a malfunction: name the branches on the typed
		// error and return it UNWRAPPED in shape, so errors.As reaches it and
		// the caller can render the paths rather than a nested string.
		var conflict *MergeConflictError
		if errors.As(err, &conflict) {
			conflict.Src, conflict.Dst = src, dst
			// The three commits the caller reads each conflicting fact at.
			// Attached here rather than deeper because this is the frame that
			// knows all three, and a refusal without them leaves the caller
			// unable to see the versions that made the path a conflict.
			conflict.BaseCommit = baseCommit.Hash.String()
			conflict.SrcCommit = srcCommit.Hash.String()
			conflict.DstCommit = dstCommit.Hash.String()
			log.Info().Str("src", src).Str("dst", dst).
				Strs("paths", conflict.Paths).Msg("mergeIntoBranch: refused (conflicting paths)")
			return AgentReconcileResult{}, conflict
		}
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: three-way merge: %w", err)
	}

	if mergedTreeHash == dstCommit.TreeHash && !o.record {
		log.Info().
			Str("src", src).Str("dst", dst).
			Str("strategy", string(strategy)).
			Msg("mergeIntoBranch: no-op (merged tree identical to dst)")
		// Settled even here: a setting that kept dst's version of every
		// conflicting path DROPPED src's changes without writing a commit,
		// and the caller must be able to say so.
		return AgentReconcileResult{Mode: ModeNoop, NewTip: dstHash.String(), Settled: settled}, nil
	}
	if mergedTreeHash == dstCommit.TreeHash && o.skipMergeOnly {
		only, err := rh.onlyMergeCommits(srcHash, dstHash)
		if err != nil {
			return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: %w", err)
		}
		if only {
			log.Debug().Str("src", src).Str("dst", dst).
				Msg("mergeIntoBranch: no-op (src brings only merge commits and no change)")
			return AgentReconcileResult{Mode: ModeNoop, NewTip: dstHash.String()}, nil
		}
	}

	mc := &object.Commit{
		Author:       author,
		Committer:    committer,
		Message:      appendTrailersToParagraph(appendTrailerLines(fmt.Sprintf("merge: %s into %s (%s)", src, dst, strategy), trailers), o.trace),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{dstHash, srcHash},
	}

	mergeHash, err := storeCommit(rh.gits, signer, mc)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: merge commit: %w", err)
	}

	if err := rh.deriveBeforeAdvance(ctx, mergeHash); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: %w", err)
	}
	newRef := plumbing.NewHashReference(dstRefName, mergeHash)
	if err := rh.gits.SetReference(newRef); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: update dst ref: %w", err)
	}

	if err := rh.populateCommitLog(ctx, dst); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: populate commit_log: %w", err)
	}
	if err := rh.notifyCommit(ctx, dst, mergeHash); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: three-way notify: %w", err)
	}

	log.Info().
		Str("src", src).
		Str("dst", dst).
		Str("strategy", string(strategy)).
		Str("merge_commit", mergeHash.String()[:8]).
		Msg("mergeIntoBranch: three-way merge complete")

	return AgentReconcileResult{Mode: ModeMerge, NewTip: mergeHash.String(), Settled: settled}, nil
}

// mergeTreesWithStrategy performs a three-way tree merge anchored on
// baseCommit, applying changes from srcCommit to dstCommit's tree. The
// strategy determines conflict resolution when both src and dst modified
// the same path relative to base:
//
//   - StrategyLocalWins: dst (the branch being updated) wins conflicts. Only
//     apply a change from src if dst's version matches base (no local change)
//     OR if the change from src is a pure addition (path absent in base).
//     Deletions from src are only applied if dst's version also matches base.
//
//   - StrategyRemoteWins: src (the branch providing updates) wins conflicts.
//     Apply every change from src unconditionally. This is the exact
//     behavior of the old remoteIndex.threeWayMerge.
//
//   - StrategyRefuse: resolve NOTHING. Every conflicting path is collected
//     and the merge aborts with a *MergeConflictError, before the caller
//     writes any ref. Stricter than the other two on one case: a path SRC
//     adds that DST has already added with different content is a conflict
//     here, where LocalWins/RemoteWins both treat a pure Insert as
//     non-conflicting. A strategy whose contract is "make no choice" cannot
//     make that one silently.
//
// Non-conflicting changes are applied in both resolving strategies: additions
// and modifications where dst didn't touch the path, and deletions where dst
// didn't modify the file.
//
// Returns the hash of the merged tree.
func (rh *repoHandler) mergeTreesWithStrategy(
	ctx context.Context,
	baseCommit, srcCommit, dstCommit *object.Commit,
	strategy ConflictStrategy,
	resolutions map[string]Resolution,
) (plumbing.Hash, []string, error) {
	// Resolutions adjudicate paths that StrategyRefuse would otherwise refuse.
	// They are meaningless to a strategy that already picks a side, and
	// accepting them there would quietly imply an adjudication the caller did
	// not get.
	if len(resolutions) > 0 && strategy != StrategyRefuse {
		return plumbing.ZeroHash, nil, fmt.Errorf("merge: resolutions are only valid with %q, got %q", StrategyRefuse, strategy)
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("base tree: %w", err)
	}
	srcTree, err := srcCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("src tree: %w", err)
	}
	dstTree, err := dstCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("dst tree: %w", err)
	}

	changes, err := object.DiffTree(baseTree, srcTree)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("diff tree: %w", err)
	}

	if len(changes) == 0 {
		return dstTree.Hash, nil, nil
	}

	currentTree := dstTree
	// Under StrategyRefuse this collects every conflicting path so the caller
	// is told all of them at once; the other strategies never append to it.
	// Sorted at the end so the reported set — and any test asserting it — does
	// not depend on DiffTree's walk order.
	var conflicts []string
	// Conflicting paths with NO version at the merge base — both sides added
	// them. Tracked separately because a reader must not be sent to read a
	// base version that does not exist (see MergeConflictError.PathsWithoutBase).
	var noBase []string
	// record is the Knomit-Conflict line of every conflict a resolving
	// strategy settles by picking a side (never under Refuse, whose conflicts
	// are refused or adjudicated by the caller, who records its own).
	var record []string
	pickSide := func(path string, kept ResolutionSide, base, srcBlob, dstBlob plumbing.Hash) {
		record = append(record, conflictLine(conflictShape{path: path, base: base, src: srcBlob, dst: dstBlob}, kept, strategy, ""))
	}

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("change action: %w", err)
		}

		switch action {
		case merkletrie.Insert:
			// Pure addition in src relative to base. Not a conflict for the
			// resolving strategies — apply in both. (If dst independently
			// added the same path with a different blob, RemoteWins
			// overwrites and LocalWins would also overwrite per the current
			// semantics; the spec treats pure Insert as non-conflicting.)
			path := change.To.Name
			blobHash := change.To.TreeEntry.Hash
			if strategy == StrategyRefuse {
				// The one case refuse judges differently: dst already holds
				// this path with different bytes, so applying src's addition
				// would overwrite an edit nobody adjudicated. A non-clashing
				// addition falls through and is applied like anywhere else —
				// refuse aborts on conflicts, it does not decline clean work.
				if dstBlob, dstHas := treeBlobHash(dstTree, path); dstHas && dstBlob != blobHash {
					res, ok := resolutions[path]
					if !ok {
						conflicts = append(conflicts, path)
						// A dual add: src and dst both created this path, so
						// the base has no version of it.
						noBase = append(noBase, path)
						continue
					}
					currentTree, err = rh.applyResolution(currentTree, path, res, blobHash, false)
					if err != nil {
						return plumbing.ZeroHash, nil, err
					}
					continue
				}
			}
			if strategy != StrategyRefuse {
				if dstBlob, dstHas := treeBlobHash(dstTree, path); dstHas && dstBlob != blobHash {
					pickSide(path, ResolveSrc, plumbing.ZeroHash, blobHash, dstBlob)
				}
			}
			newRootHash, err := buildTree(rh.gits, currentTree, path, blobHash)
			if err != nil {
				return plumbing.ZeroHash, nil, fmt.Errorf("apply insert %q: %w", path, err)
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, nil, fmt.Errorf("reload tree after insert %q: %w", path, err)
			}

		case merkletrie.Modify:
			path := change.To.Name
			blobHash := change.To.TreeEntry.Hash

			baseHash, baseHas := treeBlobHash(baseTree, path)
			dstHashAtPath, dstHas := treeBlobHash(dstTree, path)

			conflict := false
			if !dstHas {
				// dst deleted a file that src modified.
				conflict = true
			} else if baseHas && dstHashAtPath != baseHash {
				// dst modified the file relative to base.
				conflict = true
			}

			if conflict && strategy == StrategyRefuse {
				res, ok := resolutions[path]
				if !ok {
					conflicts = append(conflicts, path)
					if !baseHas {
						noBase = append(noBase, path)
					}
					continue
				}
				currentTree, err = rh.applyResolution(currentTree, path, res, blobHash, false)
				if err != nil {
					return plumbing.ZeroHash, nil, err
				}
				continue
			}
			// Both sides making the SAME change drops nothing.
			sameChange := dstHas && dstHashAtPath == blobHash
			if conflict && strategy == StrategyLocalWins {
				log.Debug().Str("path", path).Msg("merge: LocalWins skips src modify (dst wins)")
				if !sameChange {
					pickSide(path, ResolveDst, baseHash, blobHash, dstHashAtPath)
				}
				continue
			}
			if conflict && strategy == StrategyRemoteWins {
				log.Info().Str("path", path).Msg("merge: RemoteWins overwrites dst change")
				if !sameChange {
					pickSide(path, ResolveSrc, baseHash, blobHash, dstHashAtPath)
				}
			}

			newRootHash, err := buildTree(rh.gits, currentTree, path, blobHash)
			if err != nil {
				return plumbing.ZeroHash, nil, fmt.Errorf("apply modify %q: %w", path, err)
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, nil, fmt.Errorf("reload tree after modify %q: %w", path, err)
			}

		case merkletrie.Delete:
			path := change.From.Name

			baseHash, baseHas := treeBlobHash(baseTree, path)
			dstHashAtPath, dstHas := treeBlobHash(dstTree, path)

			if !dstHas {
				// dst already deleted the file — no-op in both strategies.
				continue
			}

			conflict := baseHas && dstHashAtPath != baseHash
			if conflict && strategy == StrategyRefuse {
				res, ok := resolutions[path]
				if !ok {
					conflicts = append(conflicts, path)
					// A Delete conflict requires baseHas, so the base always
					// has a version here — nothing to record.
					continue
				}
				// src DELETED this path, so "take src" means delete it.
				currentTree, err = rh.applyResolution(currentTree, path, res, plumbing.ZeroHash, true)
				if err != nil {
					return plumbing.ZeroHash, nil, err
				}
				continue
			}
			if conflict && strategy == StrategyLocalWins {
				log.Debug().Str("path", path).Msg("merge: LocalWins skips src delete (dst modified)")
				pickSide(path, ResolveDst, baseHash, plumbing.ZeroHash, dstHashAtPath)
				continue
			}
			if conflict && strategy == StrategyRemoteWins {
				log.Warn().Str("path", path).Msg("merge: RemoteWins deletes dst-modified file")
				pickSide(path, ResolveSrc, baseHash, plumbing.ZeroHash, dstHashAtPath)
			}

			newRootHash, err := deleteFromTree(rh.gits, currentTree, path)
			if err != nil {
				// dstHas was confirmed above, so the path SHOULD be present
				// in currentTree (an earlier iteration cannot have removed
				// it — merkletrie.Change is per-path). If this fires, either
				// our invariant is wrong or the storer is failing (corrupt
				// object, encode/write error). Either case must abort the
				// merge — silently continuing would produce a tree that
				// still contains a file the strategy meant to delete.
				return plumbing.ZeroHash, nil, fmt.Errorf("merge: delete %q from tree: %w", path, err)
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, nil, fmt.Errorf("reload tree after delete %q: %w", path, err)
			}
		}
	}

	if len(conflicts) > 0 {
		// The tree built above is discarded unwritten: only loose objects were
		// created, no ref was moved, and the caller returns before it would
		// have been. Sorted so the reported set is deterministic.
		sort.Strings(conflicts)
		sort.Strings(noBase)
		// Src/Dst are filled in by the caller, which is where the branch names
		// live; this layer knows only commits.
		//
		// Note the set is only what is STILL unresolved: a retry that
		// adjudicates two of three paths is told about the third alone, not
		// about all three again. Being re-told about work already done is how
		// a caller concludes its resolutions were ignored.
		return plumbing.ZeroHash, nil, &MergeConflictError{Paths: conflicts, PathsWithoutBase: noBase}
	}

	return currentTree.Hash, record, nil
}

// detectConflicts runs the refusing walk with NO resolutions to learn which
// paths actually conflict. It writes nothing: the tree it builds is discarded,
// exactly as a refused merge discards its own.
func (rh *repoHandler) detectConflicts(
	ctx context.Context,
	baseCommit, srcCommit, dstCommit *object.Commit,
) (map[string]bool, error) {
	_, _, err := rh.mergeTreesWithStrategy(ctx, baseCommit, srcCommit, dstCommit, StrategyRefuse, nil)
	if err == nil {
		return map[string]bool{}, nil // clean merge: nothing conflicts
	}
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) {
		return nil, err
	}
	set := make(map[string]bool, len(conflict.Paths))
	for _, p := range conflict.Paths {
		set[p] = true
	}
	return set, nil
}

// noLeftoverResolutions rejects resolutions for paths that did not conflict.
//
// A caller that resolves a path which is not in conflict believes the merge is
// something other than it is. Ignoring the entry would let it conclude it had
// settled that path — so it is an error naming exactly which ones.
func noLeftoverResolutions(resolutions map[string]Resolution, conflicts map[string]bool) error {
	var leftover []string
	for path := range resolutions {
		if !conflicts[path] {
			leftover = append(leftover, path)
		}
	}
	if len(leftover) == 0 {
		return nil
	}
	sort.Strings(leftover)
	return fmt.Errorf("merge: resolution given for %d path(s) that did not conflict: %s",
		len(leftover), strings.Join(leftover, ", "))
}

// applyResolution writes one adjudicated path into the tree being built.
//
// srcBlob is what src made of the path and srcDeleted says src removed it, so
// "take src" means the same thing in every arm — including the one where src's
// version is absence.
//
// An empty Resolution is an ERROR, never a default. A caller that names a path
// without naming an answer has not adjudicated it, and picking a side for them
// is precisely the silent resolution StrategyRefuse exists to prevent.
func (rh *repoHandler) applyResolution(
	currentTree *object.Tree,
	path string,
	res Resolution,
	srcBlob plumbing.Hash,
	srcDeleted bool,
) (*object.Tree, error) {
	switch {
	case res.Body != nil:
		blobHash, err := writeBlobToStore(rh.gits, res.Body)
		if err != nil {
			return nil, fmt.Errorf("resolution body for %q: %w", path, err)
		}
		return rh.setPath(currentTree, path, blobHash)

	case res.Side == ResolveSrc:
		if srcDeleted {
			newRoot, err := deleteFromTree(rh.gits, currentTree, path)
			if err != nil {
				return nil, fmt.Errorf("resolution src-delete %q: %w", path, err)
			}
			return object.GetTree(rh.gits, newRoot)
		}
		return rh.setPath(currentTree, path, srcBlob)

	case res.Side == ResolveDst:
		// dst's version is already what currentTree holds — the resolution is
		// to leave it alone. Returning the tree unchanged is the whole action.
		return currentTree, nil

	default:
		return nil, fmt.Errorf("resolution for %q names neither a side nor a body", path)
	}
}

// setPath puts blobHash at path in the tree and reloads it.
func (rh *repoHandler) setPath(currentTree *object.Tree, path string, blobHash plumbing.Hash) (*object.Tree, error) {
	newRoot, err := buildTree(rh.gits, currentTree, path, blobHash)
	if err != nil {
		return nil, fmt.Errorf("apply resolution %q: %w", path, err)
	}
	return object.GetTree(rh.gits, newRoot)
}

// treeBlobHash looks up path in tree. Returns (hash, true) if the path
// resolves to a file entry, (zero, false) if not found.
func treeBlobHash(tree *object.Tree, path string) (plumbing.Hash, bool) {
	f, err := tree.File(path)
	if err != nil {
		return plumbing.ZeroHash, false
	}
	return f.Hash, true
}
