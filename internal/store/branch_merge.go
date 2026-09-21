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
// reported as ModeNoop rather than synthesizing a zero-diff merge commit —
// this preserves the commit-log parity invariant.
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
	if isDstAncestor {
		// A fast-forward takes src wholesale: nothing conflicted, so a
		// resolution here adjudicated nothing. Checked BEFORE the ref moves —
		// a rejected call must leave the branch exactly where it was.
		if err := noLeftoverResolutions(resolutions, nil); err != nil {
			return AgentReconcileResult{}, err
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
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: no common ancestor between %q and %q (disjoint histories)", src, dst)
	}
	baseCommit := bases[0]

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

	mergedTreeHash, err := rh.mergeTreesWithStrategy(ctx, baseCommit, srcCommit, dstCommit, strategy, resolutions)
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

	if mergedTreeHash == dstCommit.TreeHash {
		log.Info().
			Str("src", src).Str("dst", dst).
			Str("strategy", string(strategy)).
			Msg("mergeIntoBranch: no-op (merged tree identical to dst)")
		return AgentReconcileResult{Mode: ModeNoop, NewTip: dstHash.String()}, nil
	}

	mc := &object.Commit{
		Author:       rh.authorSig(dst, "merge"),
		Committer:    rh.committerSig(dst),
		Message:      fmt.Sprintf("merge: %s into %s (%s)", src, dst, strategy),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{dstHash, srcHash},
	}

	commitObj := rh.gits.NewEncodedObject()
	if err := mc.Encode(commitObj); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: encode merge commit: %w", err)
	}
	mergeHash, err := rh.gits.SetEncodedObject(commitObj)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: store merge commit: %w", err)
	}

	mergeHash, err = signCommitInPlace(rh.gits, rh.signer, mergeHash)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("mergeIntoBranch: sign merge commit: %w", err)
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

	return AgentReconcileResult{Mode: ModeMerge, NewTip: mergeHash.String()}, nil
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
) (plumbing.Hash, error) {
	// Resolutions adjudicate paths that StrategyRefuse would otherwise refuse.
	// They are meaningless to a strategy that already picks a side, and
	// accepting them there would quietly imply an adjudication the caller did
	// not get.
	if len(resolutions) > 0 && strategy != StrategyRefuse {
		return plumbing.ZeroHash, fmt.Errorf("merge: resolutions are only valid with %q, got %q", StrategyRefuse, strategy)
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("base tree: %w", err)
	}
	srcTree, err := srcCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("src tree: %w", err)
	}
	dstTree, err := dstCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("dst tree: %w", err)
	}

	changes, err := object.DiffTree(baseTree, srcTree)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("diff tree: %w", err)
	}

	if len(changes) == 0 {
		return dstTree.Hash, nil
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

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("change action: %w", err)
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
						return plumbing.ZeroHash, err
					}
					continue
				}
			}
			newRootHash, err := buildTree(rh.gits, currentTree, path, blobHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("apply insert %q: %w", path, err)
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("reload tree after insert %q: %w", path, err)
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
					return plumbing.ZeroHash, err
				}
				continue
			}
			if conflict && strategy == StrategyLocalWins {
				log.Debug().Str("path", path).Msg("merge: LocalWins skips src modify (dst wins)")
				continue
			}
			if conflict && strategy == StrategyRemoteWins {
				log.Info().Str("path", path).Msg("merge: RemoteWins overwrites dst change")
			}

			newRootHash, err := buildTree(rh.gits, currentTree, path, blobHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("apply modify %q: %w", path, err)
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("reload tree after modify %q: %w", path, err)
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
					return plumbing.ZeroHash, err
				}
				continue
			}
			if conflict && strategy == StrategyLocalWins {
				log.Debug().Str("path", path).Msg("merge: LocalWins skips src delete (dst modified)")
				continue
			}
			if conflict && strategy == StrategyRemoteWins {
				log.Warn().Str("path", path).Msg("merge: RemoteWins deletes dst-modified file")
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
				return plumbing.ZeroHash, fmt.Errorf("merge: delete %q from tree: %w", path, err)
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("reload tree after delete %q: %w", path, err)
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
		return plumbing.ZeroHash, &MergeConflictError{Paths: conflicts, PathsWithoutBase: noBase}
	}

	return currentTree.Hash, nil
}

// detectConflicts runs the refusing walk with NO resolutions to learn which
// paths actually conflict. It writes nothing: the tree it builds is discarded,
// exactly as a refused merge discards its own.
func (rh *repoHandler) detectConflicts(
	ctx context.Context,
	baseCommit, srcCommit, dstCommit *object.Commit,
) (map[string]bool, error) {
	_, err := rh.mergeTreesWithStrategy(ctx, baseCommit, srcCommit, dstCommit, StrategyRefuse, nil)
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
