// Real git merge for MergeBranch and the strategy-aware three-way tree merge
// helper shared with remoteIndex.Sync. Lives on repoHandler because all the
// inputs (git storer, repo, signer, branch lock, commit-log plumbing) are
// rooted there.
package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/rs/zerolog/log"
)

// MergeBranch merges src into dst using the given conflict strategy.
// Creates a real git merge commit with two parents (dst and src) when the
// histories have diverged, a fast-forward when dst is an ancestor of src,
// and a no-op when src is already an ancestor of dst (or the refs match).
//
// The strategy controls conflict resolution for paths that both branches
// modified relative to their common ancestor — see mergeTreesWithStrategy
// for the exact semantics. If strategy is empty, StrategyLocalWins is used.
//
// Note: MergeBranch only updates git state and commit_log; callers that
// also maintain a search/fact index must trigger an index sync for dst
// afterwards (mirroring what WriteFact does internally via fi.im.Sync).
func (rh *repoHandler) MergeBranch(ctx context.Context, src, dst string, strategy ConflictStrategy) error {
	if strategy == "" {
		strategy = StrategyLocalWins
	}

	// Ensure both branches are registered in the SQLite branches table. Both
	// must exist as git refs — merging into a non-existent branch is a
	// use-after error, not a valid operation.
	if _, err := rh.branchID(ctx, src); err != nil {
		return fmt.Errorf("MergeBranch: src %q: %w", src, err)
	}
	if _, err := rh.branchID(ctx, dst); err != nil {
		return fmt.Errorf("MergeBranch: dst %q: %w", dst, err)
	}

	unlock := rh.lockBranch(dst)
	defer unlock()

	srcRefName := plumbing.NewBranchReferenceName(src)
	dstRefName := plumbing.NewBranchReferenceName(dst)

	srcRef, err := rh.gits.Reference(srcRefName)
	if err != nil {
		return fmt.Errorf("MergeBranch: resolve src ref %q: %w", src, err)
	}
	dstRef, err := rh.gits.Reference(dstRefName)
	if err != nil {
		return fmt.Errorf("MergeBranch: resolve dst ref %q: %w", dst, err)
	}
	srcHash := srcRef.Hash()
	dstHash := dstRef.Hash()

	// Same-hash no-op.
	if srcHash == dstHash {
		return nil
	}

	srcCommit, err := rh.repo.CommitObject(srcHash)
	if err != nil {
		return fmt.Errorf("MergeBranch: src commit: %w", err)
	}
	dstCommit, err := rh.repo.CommitObject(dstHash)
	if err != nil {
		return fmt.Errorf("MergeBranch: dst commit: %w", err)
	}

	// Already-merged: src is an ancestor of dst → no-op.
	isSrcAncestor, err := srcCommit.IsAncestor(dstCommit)
	if err != nil {
		return fmt.Errorf("MergeBranch: check src ancestor: %w", err)
	}
	if isSrcAncestor {
		return nil
	}

	// Fast-forward: dst is an ancestor of src → advance dst to src.
	isDstAncestor, err := dstCommit.IsAncestor(srcCommit)
	if err != nil {
		return fmt.Errorf("MergeBranch: check dst ancestor: %w", err)
	}
	if isDstAncestor {
		newRef := plumbing.NewHashReference(dstRefName, srcHash)
		if err := rh.gits.SetReference(newRef); err != nil {
			return fmt.Errorf("MergeBranch: fast-forward ref: %w", err)
		}
		if err := rh.populateCommitLog(ctx, dst); err != nil {
			log.Warn().Err(err).Msg("MergeBranch: fast-forward populate failed")
		}
		rh.notifyCommit(ctx, dst, srcHash)
		log.Info().
			Str("src", src).Str("dst", dst).
			Str("to", srcHash.String()[:8]).
			Msg("MergeBranch: fast-forward")
		return nil
	}

	// Three-way merge.
	bases, err := dstCommit.MergeBase(srcCommit)
	if err != nil {
		return fmt.Errorf("MergeBranch: merge base: %w", err)
	}
	if len(bases) == 0 {
		return fmt.Errorf("MergeBranch: no common ancestor between %q and %q (disjoint histories)", src, dst)
	}
	baseCommit := bases[0]

	mergedTreeHash, err := rh.mergeTreesWithStrategy(ctx, baseCommit, srcCommit, dstCommit, strategy)
	if err != nil {
		return fmt.Errorf("MergeBranch: three-way merge: %w", err)
	}

	// If the merged tree is identical to dst's tree, every src change was
	// either a no-op or skipped by the conflict strategy. Creating a merge
	// commit would be noise (and would violate the commit-log parity
	// invariant, which requires every reachable commit to have at least one
	// changed-file entry). Treat this as a successful no-op merge.
	if mergedTreeHash == dstCommit.TreeHash {
		log.Info().
			Str("src", src).Str("dst", dst).
			Str("strategy", string(strategy)).
			Msg("MergeBranch: no-op (merged tree identical to dst)")
		return nil
	}

	// Create merge commit with two parents: dst first ("ours"), src second ("theirs").
	mc := &object.Commit{
		Author:       rh.authorSig(dst, "merge"),
		Committer:    rh.committerSig(dst),
		Message:      fmt.Sprintf("merge: %s into %s (%s)", src, dst, strategy),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{dstHash, srcHash},
	}

	commitObj := rh.gits.NewEncodedObject()
	if err := mc.Encode(commitObj); err != nil {
		return fmt.Errorf("MergeBranch: encode merge commit: %w", err)
	}
	mergeHash, err := rh.gits.SetEncodedObject(commitObj)
	if err != nil {
		return fmt.Errorf("MergeBranch: store merge commit: %w", err)
	}

	mergeHash, err = signCommitInPlace(rh.gits, rh.signer, mergeHash)
	if err != nil {
		return fmt.Errorf("MergeBranch: sign merge commit: %w", err)
	}

	newRef := plumbing.NewHashReference(dstRefName, mergeHash)
	if err := rh.gits.SetReference(newRef); err != nil {
		return fmt.Errorf("MergeBranch: update dst ref: %w", err)
	}

	if err := rh.populateCommitLog(ctx, dst); err != nil {
		log.Warn().Err(err).Msg("MergeBranch: populate commit_log failed")
	}
	rh.notifyCommit(ctx, dst, mergeHash)

	log.Info().
		Str("src", src).
		Str("dst", dst).
		Str("strategy", string(strategy)).
		Str("merge_commit", mergeHash.String()[:8]).
		Msg("MergeBranch: three-way merge complete")

	return nil
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
// Non-conflicting changes are applied in both strategies: additions and
// modifications where dst didn't touch the path, and deletions where dst
// didn't modify the file.
//
// Returns the hash of the merged tree.
func (rh *repoHandler) mergeTreesWithStrategy(
	ctx context.Context,
	baseCommit, srcCommit, dstCommit *object.Commit,
	strategy ConflictStrategy,
) (plumbing.Hash, error) {
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

	for _, change := range changes {
		action, err := change.Action()
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("change action: %w", err)
		}

		switch action {
		case merkletrie.Insert:
			// Pure addition in src relative to base. Not a conflict — apply
			// in both strategies. (If dst independently added the same path
			// with a different blob, RemoteWins overwrites and LocalWins
			// would also overwrite per the current semantics; the spec
			// treats pure Insert as non-conflicting.)
			path := change.To.Name
			blobHash := change.To.TreeEntry.Hash
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
			if conflict && strategy == StrategyLocalWins {
				log.Debug().Str("path", path).Msg("merge: LocalWins skips src delete (dst modified)")
				continue
			}
			if conflict && strategy == StrategyRemoteWins {
				log.Warn().Str("path", path).Msg("merge: RemoteWins deletes dst-modified file")
			}

			newRootHash, err := deleteFromTree(rh.gits, currentTree, path)
			if err != nil {
				// Path not in current tree — skip (shouldn't happen because
				// we already confirmed dstHas, but be defensive).
				log.Debug().Str("path", path).Err(err).Msg("merge: skip delete (not in current tree)")
				continue
			}
			currentTree, err = object.GetTree(rh.gits, newRootHash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("reload tree after delete %q: %w", path, err)
			}
		}
	}

	return currentTree.Hash, nil
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
