// Remote synchronization: Sync orchestrates the reconcile primitives
// (reconcileMain + reconcileAgent) declared in remote_reconcile.go.
// Push retains a fetch-merge-retry loop on ref conflicts via syncLocked;
// Task 9 will rewrite Push to use the reconcile primitives directly.
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

// Sync runs one reconcile cycle for the agent branch:
//
//  1. Fetch origin (configured refspecs: main + agent/<host>).
//  2. Reconcile local main to origin/main (fast-forward or force-update on rewind).
//  3. Reconcile the agent branch against its upstream (origin/agent/<host>
//     if present, else origin/main).
//
// When reconcileMain reports Rewound, the agent still reconciles correctly
// because reconcileAgent reads the (new) local main via origin/main as
// fallback upstream when origin/agent/<host> isn't present. Main is
// reconciled FIRST so the agent sees the post-fetch tip.
//
// Safe to call repeatedly; each step is a no-op when there's nothing to do.
func (ri *remoteIndex) Sync(ctx context.Context, agentBranch string, auth transport.AuthMethod) (res SyncResult, retErr error) {
	remote, err := ri.GetRemote("origin")
	if err != nil || remote == nil {
		log.Debug().Msg("Sync: no origin remote configured, skipping")
		return SyncResult{}, nil
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

	// Check if origin remote exists in git config.
	if _, err := ri.rh.repo.Remote("origin"); err != nil {
		log.Debug().Msg("Sync: no origin git remote configured, skipping")
		return SyncResult{}, nil
	}

	// Fetch using the configured refspecs (Task 1 wrote two: main + agent).
	if err := ri.rh.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
	}); err != nil && err != gogit.NoErrAlreadyUpToDate {
		return SyncResult{}, fmt.Errorf("Sync: fetch: %w", err)
	}

	return ri.reconcileNow(ctx, agentBranch)
}

// reconcileNow runs the post-fetch portion of Sync. Exposed (package-private)
// for tests that want to set up refs manually without a real remote.
//
// Acquires rh.lockBranch("main") for reconcileMain and releases it before
// reconcileAgent acquires rh.lockBranch(agentBranch). This avoids holding
// two branch locks simultaneously.
func (ri *remoteIndex) reconcileNow(ctx context.Context, agentBranch string) (SyncResult, error) {
	mainUnlock := ri.rh.lockBranch("main")
	mainRes, err := ri.rh.reconcileMain(ctx)
	mainUnlock()
	if err != nil {
		return SyncResult{Main: mainRes}, fmt.Errorf("Sync: reconcileMain: %w", err)
	}

	agentRes, err := ri.rh.reconcileAgent(ctx, agentBranch, StrategyLocalWins)
	if err != nil {
		return SyncResult{Main: mainRes, Agent: agentRes}, fmt.Errorf("Sync: reconcileAgent: %w", err)
	}

	return SyncResult{Main: mainRes, Agent: agentRes}, nil
}

// syncLocked fetches from origin and merges origin/<remoteBranch> into
// <localBranch> using the given conflict strategy.
//
// TRANSITIONAL: this is the legacy three-way-merge sync, retained ONLY
// for Push's retry-on-conflict path. Task 9 will rewrite Push to use the
// reconcile primitives directly, at which point syncLocked,
// isRefUpdateConflict, maxPushAttempts, and SetMaxPushAttemptsForTest
// can be removed. The public Sync entry point already routes through
// reconcileNow, not this helper.
//
// The caller must hold ri.rh.lockBranch(localBranch). This helper does NOT
// update the remote status row — that is the outer caller's responsibility
// (Push writes push-status).
func (ri *remoteIndex) syncLocked(
	ctx context.Context,
	localBranch, remoteBranch string,
	auth transport.AuthMethod,
	strategy ConflictStrategy,
) error {
	log.Debug().Msg("git sync: fetching from origin")
	err := ri.rh.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("Sync: fetch: %w", err)
	}

	// Resolve origin/<remoteBranch> ref.
	originRef, err := ri.rh.gits.Reference(plumbing.NewRemoteReferenceName("origin", remoteBranch))
	if err != nil {
		log.Debug().Str("branch", remoteBranch).Msg("git sync: origin ref not found, skipping")
		return nil
	}
	originHash := originRef.Hash()

	// Get current local branch HEAD.
	agentRefName := plumbing.NewBranchReferenceName(localBranch)
	agentRef, err := ri.rh.gits.Reference(agentRefName)
	if err != nil {
		return fmt.Errorf("Sync: agent ref: %w", err)
	}
	agentHash := agentRef.Hash()

	log.Debug().
		Str("origin", originHash.String()[:8]).
		Str("agent", agentHash.String()[:8]).
		Str("branch", localBranch).
		Msg("git sync: comparing refs")

	// Same hash — no-op.
	if originHash == agentHash {
		return nil
	}

	originCommit, err := ri.rh.repo.CommitObject(originHash)
	if err != nil {
		return fmt.Errorf("Sync: origin commit: %w", err)
	}

	agentCommit, err := ri.rh.repo.CommitObject(agentHash)
	if err != nil {
		return fmt.Errorf("Sync: agent commit: %w", err)
	}

	// Check if origin is already an ancestor of agent (already merged).
	isOriginAncestor, err := originCommit.IsAncestor(agentCommit)
	if err != nil {
		return fmt.Errorf("Sync: check origin ancestor: %w", err)
	}
	if isOriginAncestor {
		log.Debug().Msg("git sync: origin already merged, nothing to do")
		return nil
	}

	// Check if agent HEAD is ancestor of origin → fast-forward.
	isAgentAncestor, err := agentCommit.IsAncestor(originCommit)
	if err != nil {
		return fmt.Errorf("Sync: check agent ancestor: %w", err)
	}
	if isAgentAncestor {
		newRef := plumbing.NewHashReference(agentRefName, originHash)
		if err := ri.rh.gits.SetReference(newRef); err != nil {
			return fmt.Errorf("Sync: fast-forward ref: %w", err)
		}

		log.Info().Str("to", originHash.String()[:8]).Msg("git sync: fast-forward")
		if err := ri.rh.populateCommitLog(ctx, localBranch); err != nil {
			log.Warn().Err(err).Msg("commit_log: sync populate")
		}
		// notifyCommit runs inside the branch lock — see fact_write.go writeFile
		// for rationale.
		if err := ri.rh.notifyCommit(ctx, localBranch, originHash); err != nil {
			return fmt.Errorf("Sync: fast-forward notify: %w", err)
		}
		return nil
	}

	// Find merge base.
	bases, err := agentCommit.MergeBase(originCommit)
	if err != nil {
		return fmt.Errorf("Sync: merge base: %w", err)
	}
	if len(bases) == 0 {
		return fmt.Errorf("Sync: no common ancestor found (disjoint histories)")
	}
	baseCommit := bases[0]

	log.Debug().Str("base", baseCommit.Hash.String()[:8]).Msg("git sync: merge base")

	// Three-way merge: diff base→origin, apply to agent tree. Conflict
	// resolution is per the caller's strategy:
	//   - StrategyLocalWins:  used by Push retry. The pusher's changes are
	//     authoritative; origin's concurrent changes are preserved for
	//     non-overlapping paths.
	mergedTreeHash, err := ri.rh.mergeTreesWithStrategy(ctx, baseCommit, originCommit, agentCommit, strategy)
	if err != nil {
		return fmt.Errorf("Sync: three-way merge: %w", err)
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
		return fmt.Errorf("Sync: encode merge commit: %w", err)
	}
	mergeHash, err := ri.rh.gits.SetEncodedObject(commitObj)
	if err != nil {
		return fmt.Errorf("Sync: store merge commit: %w", err)
	}

	mergeHash, err = signCommitInPlace(ri.rh.gits, ri.rh.signer, mergeHash)
	if err != nil {
		return fmt.Errorf("Sync: sign merge commit: %w", err)
	}

	newRef := plumbing.NewHashReference(agentRefName, mergeHash)
	if err := ri.rh.gits.SetReference(newRef); err != nil {
		return fmt.Errorf("Sync: update ref: %w", err)
	}

	log.Info().Str("merge_commit", mergeHash.String()[:8]).Msg("git sync: merged origin")
	if err := ri.rh.populateCommitLog(ctx, localBranch); err != nil {
		log.Warn().Err(err).Msg("commit_log: sync populate")
	}
	// notifyCommit runs inside the branch lock — see fact_write.go writeFile.
	if err := ri.rh.notifyCommit(ctx, localBranch, mergeHash); err != nil {
		return fmt.Errorf("Sync: merge notify: %w", err)
	}
	return nil
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
		if merr := ri.syncLocked(ctx, branch, branch, auth, StrategyLocalWins); merr != nil {
			return PushResult{}, fmt.Errorf("Push: reconcile after conflict: %w", merr)
		}
	}

	return PushResult{}, fmt.Errorf("Push: exhausted %d attempts after conflict: %w", maxPushAttempts, lastErr)
}
