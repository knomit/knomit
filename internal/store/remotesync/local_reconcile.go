// Local reconcile: keeping the consensus branch meaningful on a repo that has
// no origin to follow.
//
// A repo WITH an origin gets its consensus branch from the remote —
// reconcileMain fast-forwards refs/heads/<upstream> to
// refs/remotes/origin/<upstream> on every sync tick. A repo WITHOUT one never
// starts that loop, so its main sat at the root commit forever while every
// fact accumulated on the agent branch. That is invisible locally (readers
// follow the agent branch) and fatal to a peer: a knomit instance subscribing
// to this one over /git reads the consensus branch, and would have seen one
// empty commit.
package remotesync

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/merge"
)

// EnsureLocalUpstream creates refs/heads/<upstream> from
// refs/remotes/origin/<upstream> when the local ref is missing; the contract
// is Service.EnsureLocalUpstream's. This is the body of that method, moved
// statement for statement.
func (rh *Repo) EnsureLocalUpstream(ctx context.Context, upstream string) (bool, error) {
	if upstream == "" {
		return false, nil
	}
	unlock := rh.LockBranch(upstream)
	defer unlock()

	local := plumbing.NewBranchReferenceName(upstream)
	if _, err := rh.Storer().Reference(local); err == nil {
		return false, nil
	}
	originRef, err := rh.Storer().Reference(plumbing.NewRemoteReferenceName("origin", upstream))
	if err != nil {
		return false, nil
	}
	if err := rh.DeriveBeforeAdvance(ctx, originRef.Hash()); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: %w", err)
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(local, originRef.Hash())); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: create %s: %w", upstream, err)
	}
	if _, err := rh.EnsureBranch(ctx, upstream, "refs/heads/"+upstream); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: ensure %s: %w", upstream, err)
	}
	if err := rh.PopulateCommitLog(ctx, upstream); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: populate commit_log: %w", err)
	}
	if err := rh.NotifyCommit(ctx, upstream, originRef.Hash()); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: notify: %w", err)
	}
	log.Info().Str("upstream", upstream).Str("at", originRef.Hash().String()[:8]).
		Msg("EnsureLocalUpstream: bootstrapped local upstream from origin")
	return true, nil
}

// AdvanceLocalUpstream fast-forwards refs/heads/<upstream> to the agent tip on
// a repo that has NO origin; the contract (it NEVER resets) is
// Service.AdvanceLocalUpstream's. This is the body of that method, moved
// statement for statement.
func (rh *Repo) AdvanceLocalUpstream(ctx context.Context, agentBranch, upstream string) (MainReconcileResult, error) {
	if agentBranch == "" || upstream == "" || agentBranch == upstream {
		return MainReconcileResult{Mode: merge.ModeNoop}, nil
	}
	// Only the upstream is written; the agent branch is read. One lock, the
	// same one reconcileMain runs under.
	unlock := rh.LockBranch(upstream)
	defer unlock()

	agentRef, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: read %s: %w", agentBranch, err)
	}
	upstreamName := plumbing.NewBranchReferenceName(upstream)
	upRef, err := rh.Storer().Reference(upstreamName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: read %s: %w", upstream, err)
	}
	if upRef.Hash() == agentRef.Hash() {
		return MainReconcileResult{Mode: merge.ModeNoop}, nil
	}
	upCommit, err := rh.Repository().CommitObject(upRef.Hash())
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %s commit: %w", upstream, err)
	}
	agentCommit, err := rh.Repository().CommitObject(agentRef.Hash())
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %s commit: %w", agentBranch, err)
	}
	ff, err := upCommit.IsAncestor(agentCommit)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: IsAncestor: %w", err)
	}
	if !ff {
		log.Warn().Str("upstream", upstream).Str("agent", agentBranch).
			Msg("AdvanceLocalUpstream: upstream is not an ancestor of the agent branch; skipping (never resets)")
		return MainReconcileResult{Mode: merge.ModeNoop}, nil
	}
	// The fleet repository's own commits are judged before its main moves
	// (JudgeOwnRange; every other repository returns at once). A refusal
	// leaves main where it is.
	own, _ := rh.OwnAgentID(agentBranch)
	if err := rh.JudgeOwnRange(upstream, upRef.Hash(), agentRef.Hash(), own, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %w", err)
	}
	if _, err := rh.EnsureBranch(ctx, upstream, "refs/heads/"+upstream); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: ensure %s: %w", upstream, err)
	}
	if err := rh.DeriveBeforeAdvance(ctx, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %w", err)
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(upstreamName, agentRef.Hash())); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: fast-forward: %w", err)
	}
	if err := rh.PopulateCommitLog(ctx, upstream); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: populate commit_log after fast-forward: %w", err)
	}
	if err := rh.NotifyCommit(ctx, upstream, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: notify after fast-forward: %w", err)
	}
	log.Info().Str("upstream", upstream).Str("agent", agentBranch).
		Str("to", agentRef.Hash().String()[:8]).Msg("AdvanceLocalUpstream: fast-forward")
	return MainReconcileResult{Mode: merge.ModeFF, NewTip: agentRef.Hash().String()}, nil
}
