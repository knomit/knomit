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

	"knomit/internal/engine/store/merge"
)

// EnsureLocalUpstream creates refs/heads/<upstream> from
// refs/remotes/origin/<upstream> when the local ref is missing — a store moved
// between machines, or one written before local init bootstrapped it. This is
// reconcileMain's "create" branch, callable at open without a fetch, so the
// served advertisement has a HEAD to point at from the first request. A live
// instance was found in exactly this state: no refs/heads/main at all, only
// refs/remotes/origin/main.
//
// It never MOVES an existing upstream: advancing one is reconcileMain's
// decision and needs the ancestry check this function does not make.
// Nothing to bootstrap from is not an error — the reconcile loop's first
// fetch creates it. An empty upstream is not an error either: it creates
// nothing and answers (false, nil).
//
// It is safe to retry after a failure at any step (#587). The ref is written
// before its SQL bookkeeping (branches row, commit_log, index notify), and
// nothing undoes a SetReference when a later step fails, so an existing ref
// is not proof the bootstrap finished: when refs/heads/<upstream> is present
// but its branches row is missing or its tip is not recorded on the branch,
// the bookkeeping is completed at the ref's CURRENT hash (the ref is never
// moved) and the answer is (true, nil). An upstream whose row and tip are
// both recorded is left alone with (false, nil).
//
// ctx is checked once the branch lock is held, before anything is written:
// LockBranch is not ctx-aware, so a caller cancelled while parked behind an
// index Rebuild of the upstream (the Sync life at Unmount) must leave without
// writing rather than take the lock with a dead ctx and fail halfway.
func (ri *Index) EnsureLocalUpstream(ctx context.Context, upstream string) (bool, error) {
	if upstream == "" {
		return false, nil
	}
	rh := ri.rh
	unlock := rh.vr.LockBranch(upstream)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: %w", err)
	}

	local := plumbing.NewBranchReferenceName(upstream)
	if ref, err := rh.vr.Storer().Reference(local); err == nil {
		recorded, err := rh.vr.CommitRecorded(ctx, upstream, ref.Hash())
		if err != nil {
			return false, fmt.Errorf("EnsureLocalUpstream: %w", err)
		}
		if recorded {
			return false, nil
		}
		if err := ri.recordLocalUpstream(ctx, upstream, ref.Hash()); err != nil {
			return false, err
		}
		log.Info().Str("upstream", upstream).Str("at", ref.Hash().String()[:8]).
			Msg("EnsureLocalUpstream: completed a half-written local upstream")
		return true, nil
	}
	originRef, err := rh.vr.Storer().Reference(plumbing.NewRemoteReferenceName("origin", upstream))
	if err != nil {
		return false, nil
	}
	if err := rh.vr.DeriveBeforeAdvance(ctx, originRef.Hash()); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: %w", err)
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(local, originRef.Hash())); err != nil {
		return false, fmt.Errorf("EnsureLocalUpstream: create %s: %w", upstream, err)
	}
	if err := ri.recordLocalUpstream(ctx, upstream, originRef.Hash()); err != nil {
		return false, err
	}
	log.Info().Str("upstream", upstream).Str("at", originRef.Hash().String()[:8]).
		Msg("EnsureLocalUpstream: bootstrapped local upstream from origin")
	return true, nil
}

// recordLocalUpstream is the SQL half of EnsureLocalUpstream, run under the
// upstream's branch lock once refs/heads/<upstream> points at tip: the
// branches row, commit_log for every reachable commit, then the index notify.
// Every step is idempotent, so a run interrupted anywhere is completed by the
// next call.
func (ri *Index) recordLocalUpstream(ctx context.Context, upstream string, tip plumbing.Hash) error {
	rh := ri.rh
	if _, err := rh.vr.EnsureBranch(ctx, upstream, "refs/heads/"+upstream); err != nil {
		return fmt.Errorf("EnsureLocalUpstream: ensure %s: %w", upstream, err)
	}
	if err := rh.vr.PopulateCommitLog(ctx, upstream); err != nil {
		return fmt.Errorf("EnsureLocalUpstream: populate commit_log: %w", err)
	}
	if err := rh.vr.NotifyCommit(ctx, upstream, tip); err != nil {
		return fmt.Errorf("EnsureLocalUpstream: notify: %w", err)
	}
	return nil
}

// AdvanceLocalUpstream fast-forwards refs/heads/<upstream> to the agent tip on
// a repo that has NO origin, so main is a real consensus branch there too.
// Same shape as reconcileMain's fast-forward — SetReference, PopulateCommitLog,
// NotifyCommit under the branch lock — with the agent branch in
// origin/<upstream>'s role.
//
// It NEVER resets. Diverged history is logged and skipped: a repo whose
// consensus branch is not an ancestor of its agent branch carries something on
// main that the agent branch does not, and force-updating it is exactly the
// failure recorded in kb/gotchas/repos/remote-sync — a degenerate upstream
// destroyed just-written, unpushed facts. The degenerate configuration itself
// (upstream IS the agent branch) is a no-op here for the same reason, as is an
// empty agentBranch or upstream.
func (ri *Index) AdvanceLocalUpstream(ctx context.Context, agentBranch, upstream string) (MainReconcileResult, error) {
	if agentBranch == "" || upstream == "" || agentBranch == upstream {
		return MainReconcileResult{Mode: merge.ModeNoop}, nil
	}
	rh := ri.rh
	// Only the upstream is written; the agent branch is read. One lock, the
	// same one reconcileMain runs under.
	unlock := rh.vr.LockBranch(upstream)
	defer unlock()

	agentRef, err := rh.vr.Storer().Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: read %s: %w", agentBranch, err)
	}
	upstreamName := plumbing.NewBranchReferenceName(upstream)
	upRef, err := rh.vr.Storer().Reference(upstreamName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: read %s: %w", upstream, err)
	}
	if upRef.Hash() == agentRef.Hash() {
		return MainReconcileResult{Mode: merge.ModeNoop}, nil
	}
	upCommit, err := rh.vr.Repository().CommitObject(upRef.Hash())
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %s commit: %w", upstream, err)
	}
	agentCommit, err := rh.vr.Repository().CommitObject(agentRef.Hash())
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
	own, _ := rh.vr.OwnAgentID(agentBranch)
	if err := rh.JudgeOwnRange(upstream, upRef.Hash(), agentRef.Hash(), own, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %w", err)
	}
	if _, err := rh.vr.EnsureBranch(ctx, upstream, "refs/heads/"+upstream); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: ensure %s: %w", upstream, err)
	}
	if err := rh.vr.DeriveBeforeAdvance(ctx, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: %w", err)
	}
	if err := rh.vr.Storer().SetReference(plumbing.NewHashReference(upstreamName, agentRef.Hash())); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: fast-forward: %w", err)
	}
	if err := rh.vr.PopulateCommitLog(ctx, upstream); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: populate commit_log after fast-forward: %w", err)
	}
	if err := rh.vr.NotifyCommit(ctx, upstream, agentRef.Hash()); err != nil {
		return MainReconcileResult{}, fmt.Errorf("AdvanceLocalUpstream: notify after fast-forward: %w", err)
	}
	log.Info().Str("upstream", upstream).Str("agent", agentBranch).
		Str("to", agentRef.Hash().String()[:8]).Msg("AdvanceLocalUpstream: fast-forward")
	return MainReconcileResult{Mode: merge.ModeFF, NewTip: agentRef.Hash().String()}, nil
}
