// Remote synchronization: fetches from origin and merges the remote branch
// into the local branch using a common-ancestor-aware three-way merge.
package store

import (
	"context"
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
)

// maxPushAttempts bounds the total number of push attempts (initial + retries)
// in the fetch-merge-retry loop on concurrent push conflicts. Under sustained
// contention the loop eventually surfaces an error rather than livelocking.
//
// Declared as a var so tests can lower it to force exhaustion deterministically
// via SetMaxPushAttempts. Not part of the public API.
var maxPushAttempts = 5

// isRefUpdateConflict reports whether err looks like a concurrent ref-update
// race on push — the remote's branch advanced between our advertise and
// update phases, or is otherwise ahead of our local history.
func isRefUpdateConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "incorrect old value provided") ||
		strings.Contains(msg, "failed to update ref")
}

// Sync fetches from origin and merges origin/<remoteBranch> into localBranch
// using a three-way merge with "origin wins" semantics.
//
// Lock is held from fetch through ref update, then released before
// notifyCommit (which triggers index sync and may call back into Service).
//
// If remoteBranch is empty, it defaults to "main".
func (ri *remoteIndex) Sync(ctx context.Context, localBranch string, auth transport.AuthMethod) (res SyncResult, retErr error) {
	remote, err := ri.GetRemote("origin")
	if err != nil || remote == nil {
		log.Debug().Msg("git sync: no origin remote configured, skipping")
		return SyncResult{}, nil
	}
	remoteBranch := remote.Branch
	if remoteBranch == "" {
		remoteBranch = "main"
	}

	// Past the "no remote" gate — write status on every return from here.
	defer func() {
		if retErr != nil {
			errMsg := retErr.Error()
			_ = ri.updateRemoteStatus("origin", "error", &errMsg)
		} else {
			_ = ri.updateRemoteStatus("origin", "ok", nil)
		}
	}()

	unlock := ri.rh.lockBranch(localBranch)
	defer unlock()

	// Check if origin remote exists in git config.
	if _, err := ri.rh.repo.Remote("origin"); err != nil {
		log.Debug().Msg("git sync: no origin remote configured, skipping")
		return SyncResult{}, nil
	}

	return ri.syncLocked(ctx, localBranch, remoteBranch, auth, StrategyRemoteWins)
}

// syncLocked fetches from origin and merges origin/<remoteBranch> into
// <localBranch> using the given conflict strategy.
//
// The caller must hold ri.rh.lockBranch(localBranch). This helper does NOT
// update the remote status row — that is the outer caller's responsibility
// (Sync writes sync-status; Push writes push-status).
func (ri *remoteIndex) syncLocked(
	ctx context.Context,
	localBranch, remoteBranch string,
	auth transport.AuthMethod,
	strategy ConflictStrategy,
) (SyncResult, error) {
	log.Debug().Msg("git sync: fetching from origin")
	err := ri.rh.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", err)
	}

	// Resolve origin/<remoteBranch> ref.
	originRef, err := ri.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", remoteBranch))
	if err != nil {
		log.Debug().Str("branch", remoteBranch).Msg("git sync: origin ref not found, skipping")
		return SyncResult{}, nil
	}
	originHash := originRef.Hash()

	// Get current local branch HEAD.
	agentRefName := plumbing.NewBranchReferenceName(localBranch)
	agentRef, err := ri.rh.gits.Reference(agentRefName)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: agent ref: %w", err)
	}
	agentHash := agentRef.Hash()

	log.Debug().
		Str("origin", originHash.String()[:8]).
		Str("agent", agentHash.String()[:8]).
		Str("branch", localBranch).
		Msg("git sync: comparing refs")

	// Same hash — no-op.
	if originHash == agentHash {
		return SyncResult{}, nil
	}

	originCommit, err := ri.rh.repo.CommitObject(originHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: origin commit: %w", err)
	}

	agentCommit, err := ri.rh.repo.CommitObject(agentHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: agent commit: %w", err)
	}

	// Check if origin is already an ancestor of agent (already merged).
	isOriginAncestor, err := originCommit.IsAncestor(agentCommit)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: check origin ancestor: %w", err)
	}
	if isOriginAncestor {
		log.Debug().Msg("git sync: origin already merged, nothing to do")
		return SyncResult{}, nil
	}

	// Check if agent HEAD is ancestor of origin → fast-forward.
	isAgentAncestor, err := agentCommit.IsAncestor(originCommit)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: check agent ancestor: %w", err)
	}
	if isAgentAncestor {
		newRef := plumbing.NewHashReference(agentRefName, originHash)
		if err := ri.rh.gits.SetReference(newRef); err != nil {
			return SyncResult{}, fmt.Errorf("Sync: fast-forward ref: %w", err)
		}

		log.Info().Str("to", originHash.String()[:8]).Msg("git sync: fast-forward")
		if err := ri.rh.populateCommitLog(ctx, localBranch); err != nil {
			log.Warn().Err(err).Msg("commit_log: sync populate")
		}
		// notifyCommit runs inside the branch lock — see fact_write.go writeFile
		// for rationale.
		if err := ri.rh.notifyCommit(ctx, localBranch, originHash); err != nil {
			return SyncResult{}, fmt.Errorf("Sync: fast-forward notify: %w", err)
		}
		return SyncResult{Synced: true, FastForward: true}, nil
	}

	// Find merge base.
	bases, err := agentCommit.MergeBase(originCommit)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: merge base: %w", err)
	}
	if len(bases) == 0 {
		return SyncResult{}, fmt.Errorf("Sync: no common ancestor found (disjoint histories)")
	}
	baseCommit := bases[0]

	log.Debug().Str("base", baseCommit.Hash.String()[:8]).Msg("git sync: merge base")

	// Three-way merge: diff base→origin, apply to agent tree. Conflict
	// resolution is per the caller's strategy:
	//   - StrategyRemoteWins: used by Sync (pull). Origin is authoritative.
	//   - StrategyLocalWins:  used by Push retry. The pusher's changes are
	//     authoritative; origin's concurrent changes are preserved for
	//     non-overlapping paths.
	mergedTreeHash, err := ri.rh.mergeTreesWithStrategy(ctx, baseCommit, originCommit, agentCommit, strategy)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: three-way merge: %w", err)
	}

	// Create merge commit.
	mc := &object.Commit{
		Author:       ri.rh.authorSig(localBranch, "sync"),
		Committer:    ri.rh.committerSig(localBranch),
		Message:      fmt.Sprintf("sync: merge origin/%s into %s", remoteBranch, localBranch),
		TreeHash:     mergedTreeHash,
		ParentHashes: []plumbing.Hash{agentHash, originHash},
	}

	commitObj := ri.rh.gits.NewEncodedObject()
	if err := mc.Encode(commitObj); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: encode merge commit: %w", err)
	}
	mergeHash, err := ri.rh.gits.SetEncodedObject(commitObj)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: store merge commit: %w", err)
	}

	mergeHash, err = signCommitInPlace(ri.rh.gits, ri.rh.signer, mergeHash)
	if err != nil {
		return SyncResult{}, fmt.Errorf("Sync: sign merge commit: %w", err)
	}

	newRef := plumbing.NewHashReference(agentRefName, mergeHash)
	if err := ri.rh.gits.SetReference(newRef); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: update ref: %w", err)
	}

	log.Info().Str("merge_commit", mergeHash.String()[:8]).Msg("git sync: merged origin")
	if err := ri.rh.populateCommitLog(ctx, localBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: sync populate")
	}
	// notifyCommit runs inside the branch lock — see fact_write.go writeFile.
	if err := ri.rh.notifyCommit(ctx, localBranch, mergeHash); err != nil {
		return SyncResult{}, fmt.Errorf("Sync: merge notify: %w", err)
	}
	return SyncResult{Synced: true, MergeCommit: mergeHash.String()}, nil
}

// Push pushes the given branch to origin.
// Returns PushResult{Pushed: false} if already up to date.
//
// On a ref-update conflict (remote's branch advanced during our push), Push
// fetches origin, merges origin/<branch> into <branch> locally with
// StrategyLocalWins (the pusher's changes are authoritative for overlapping
// paths), then retries the push. This preserves both sides' work for
// non-overlapping paths — the common case on shared branches like main.
//
// The retry loop is bounded by maxPushAttempts to avoid livelock under
// sustained contention.
func (ri *remoteIndex) Push(ctx context.Context, branch string, auth transport.AuthMethod) (res PushResult, retErr error) {
	unlock := ri.rh.lockBranch(branch)
	defer unlock()

	if _, err := ri.rh.repo.Remote("origin"); err != nil {
		log.Debug().Msg("git push: no origin remote configured, skipping")
		return PushResult{}, nil
	}

	// Past the "no remote" gate — write status on every return from here.
	defer func() {
		if retErr != nil {
			errMsg := retErr.Error()
			_ = ri.updateRemotePushStatus("origin", "error", &errMsg)
		} else {
			_ = ri.updateRemotePushStatus("origin", "ok", nil)
		}
	}()

	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)

	var lastErr error
	for attempt := 0; attempt < maxPushAttempts; attempt++ {
		log.Debug().Str("branch", branch).Int("attempt", attempt).Msg("git push: pushing branch")
		err := ri.rh.repo.Push(&gogit.PushOptions{
			RemoteName: "origin",
			RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(refspec)},
			Auth:       auth,
		})
		if err == gogit.NoErrAlreadyUpToDate {
			return PushResult{Pushed: false}, nil
		}
		if err == nil {
			log.Info().Str("branch", branch).Msg("git push: pushed branch")
			return PushResult{Pushed: true}, nil
		}
		if !isRefUpdateConflict(err) {
			return PushResult{}, fmt.Errorf("Push: %w", err)
		}

		// Remote advanced under us. Fetch, merge origin/<branch> into
		// local <branch> with "local wins" semantics, and retry the push.
		// Skip the merge on the final attempt — no retry will follow.
		lastErr = err
		if attempt+1 >= maxPushAttempts {
			break
		}
		log.Debug().Err(err).Int("attempt", attempt).Msg("git push: ref conflict, merging remote before retry")
		if _, merr := ri.syncLocked(ctx, branch, branch, auth, StrategyLocalWins); merr != nil {
			return PushResult{}, fmt.Errorf("Push: reconcile after conflict: %w", merr)
		}
	}

	return PushResult{}, fmt.Errorf("Push: exhausted %d attempts after conflict: %w", maxPushAttempts, lastErr)
}
