// Remote synchronization: fetches from origin and merges the remote branch
// into the agent branch using a common-ancestor-aware three-way merge with
// "origin wins" semantics.
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

	// Fetch from origin.
	log.Debug().Msg("git sync: fetching from origin")
	err = ri.rh.repo.Fetch(&gogit.FetchOptions{
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

	// Get current agent branch HEAD.
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

	// Three-way merge: diff base→origin, apply to agent tree with
	// RemoteWins semantics (origin is the authoritative side for pulls).
	mergedTreeHash, err := ri.rh.mergeTreesWithStrategy(ctx, baseCommit, originCommit, agentCommit, StrategyRemoteWins)
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
// If the push fails with a non-fast-forward error, it retries with a force
// push. This is safe because agent branches are per-machine — no other machine
// writes to the same branch.
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
	forceRefspec := fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)

	log.Debug().Str("branch", branch).Msg("git push: pushing branch")
	err := ri.rh.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(refspec)},
		Auth:       auth,
	})
	if err == gogit.NoErrAlreadyUpToDate {
		return PushResult{Pushed: false}, nil
	}
	// Retry with a force-push on any non-fast-forward condition. Two
	// distinct error strings cover the same scenario:
	//
	//   - "non-fast-forward": go-git observed a non-ff ref state before
	//     the push; the remote's branch already has commits not in our
	//     local history.
	//   - "incorrect old value provided": the remote's transport did a
	//     compare-and-swap ref update and the ref advanced between our
	//     advertise and update phases (typical under concurrent pushes
	//     from two processes to the same bare remote).
	//
	// Both are safe to resolve with a force-push because agent branches
	// are per-machine — no other machine writes to the same branch — and
	// the branch-lock retry loop on the caller side ensures any local
	// commits that would be overwritten are still reachable on the losing
	// pusher's local history.
	if err != nil && (strings.Contains(err.Error(), "non-fast-forward") ||
		strings.Contains(err.Error(), "incorrect old value provided")) {
		err = ri.rh.repo.Push(&gogit.PushOptions{
			RemoteName: "origin",
			RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(forceRefspec)},
			Auth:       auth,
		})
	}
	if err != nil {
		return PushResult{}, fmt.Errorf("Push: %w", err)
	}

	log.Info().Str("branch", branch).Msg("git push: pushed branch")
	return PushResult{Pushed: true}, nil
}
