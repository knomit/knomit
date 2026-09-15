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
package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
)

// UpstreamBranch is the repo's consensus branch name: the configurable
// Remote.Branch when an origin row exists, else "main" (what local init
// creates). Served HEAD and the local reconcile both key off this.
func (s *Service) UpstreamBranch() string {
	if r, err := s.Remote().GetRemote("origin"); err == nil && r != nil && r.Branch != "" {
		return r.Branch
	}
	return "main"
}

// AdvanceLocalUpstream fast-forwards refs/heads/<upstream> to the agent tip on
// a repo that has NO origin, so main is a real consensus branch there too.
// Same shape as reconcileMain's fast-forward — SetReference, populateCommitLog,
// notifyCommit under the branch lock — with the agent branch in
// origin/<upstream>'s role.
//
// It NEVER resets. Diverged history is logged and skipped: a repo whose
// consensus branch is not an ancestor of its agent branch carries something on
// main that the agent branch does not, and force-updating it is exactly the
// failure recorded in kb/gotchas/repos/remote-sync — a degenerate upstream
// destroyed just-written, unpushed facts. The degenerate configuration itself
// (upstream IS the agent branch) is a no-op here for the same reason.
func (s *Service) AdvanceLocalUpstream(ctx context.Context, agentBranch, upstream string) (MainReconcileResult, error) {
	if agentBranch == "" || upstream == "" || agentBranch == upstream {
		return MainReconcileResult{Mode: ModeNoop}, nil
	}
	rh := s.rh
	// Only the upstream is written; the agent branch is read. One lock, the
	// same one reconcileMain runs under.
	unlock := rh.lockBranch(upstream)
	defer unlock()

	agentRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: read %s: %w", agentBranch, err)
	}
	upstreamName := plumbing.NewBranchReferenceName(upstream)
	upRef, err := rh.gits.Reference(upstreamName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: read %s: %w", upstream, err)
	}
	if upRef.Hash() == agentRef.Hash() {
		return MainReconcileResult{Mode: ModeNoop}, nil
	}
	upCommit, err := rh.repo.CommitObject(upRef.Hash())
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %s commit: %w", upstream, err)
	}
	agentCommit, err := rh.repo.CommitObject(agentRef.Hash())
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
		return MainReconcileResult{Mode: ModeNoop}, nil
	}
	if _, err := rh.EnsureBranch(ctx, upstream, "refs/heads/"+upstream); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: ensure %s: %w", upstream, err)
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(upstreamName, agentRef.Hash())); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: fast-forward: %w", err)
	}
	if err := rh.populateCommitLog(ctx, upstream); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: populate commit_log after fast-forward: %w", err)
	}
	if err := rh.notifyCommit(ctx, upstream, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: notify after fast-forward: %w", err)
	}
	log.Info().Str("upstream", upstream).Str("agent", agentBranch).
		Str("to", agentRef.Hash().String()[:8]).Msg("AdvanceLocalUpstream: fast-forward")
	return MainReconcileResult{Mode: ModeFF, NewTip: agentRef.Hash().String()}, nil
}
