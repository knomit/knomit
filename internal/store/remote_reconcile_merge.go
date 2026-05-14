// Merge-based reconcile: the steady-state path. Produces a fast-forward
// when agent is an ancestor of main, a no-op when main is an ancestor of
// agent (or hashes match), or a single merge commit when histories
// diverged. Hash rewriting NEVER happens here — that's the rebase
// fallback's job. Stable commit hashes matter because Push fast-forwards
// origin on this machine's agent branch; an unrelated rewrite would force
// an unnecessary force-push.
//
// Conflict resolution is StrategyLocalWins for steady-state sync: the
// agent's local edits win overlapping paths against main.
package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
)

// reconcileAgentMerge merges local upstreamMain into agentBranch. Holds
// rh.lockBranch(agentBranch) for the entire body so the watermark write
// is atomic with the merge — without this, a concurrent Sync could
// observe (or write) a stale watermark between the merge and the write.
//
// upstreamMain is the consensus branch name to merge from (typically "main",
// configurable to "master" etc.). Empty defaults to "main".
func (rh *repoHandler) reconcileAgentMerge(ctx context.Context, agentBranch, upstreamMain string, strategy ConflictStrategy) (AgentReconcileResult, error) {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	unlock := rh.lockBranch(agentBranch)
	defer unlock()

	res, err := rh.mergeIntoBranchLocked(ctx, upstreamMain, agentBranch, strategy)
	if err != nil {
		return res, fmt.Errorf("reconcileAgentMerge: %w", err)
	}

	mainRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(upstreamMain))
	if err != nil {
		return res, fmt.Errorf("reconcileAgentMerge: read local %s for watermark: %w", upstreamMain, err)
	}
	if err := rh.writeAgentBase(agentBranch, mainRef.Hash()); err != nil {
		return res, fmt.Errorf("reconcileAgentMerge: write watermark: %w", err)
	}

	log.Info().
		Str("branch", agentBranch).
		Str("upstream", upstreamMain).
		Str("mode", string(res.Mode)).
		Str("new_tip", shortRefHash(plumbing.NewHash(res.NewTip))).
		Msg("reconcileAgentMerge: complete")
	return res, nil
}

