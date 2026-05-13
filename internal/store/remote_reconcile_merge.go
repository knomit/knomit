// Merge-based reconcile: the steady-state path. Calls mergeIntoBranch with
// src=main, dst=agentBranch, StrategyLocalWins. Produces a fast-forward
// when agent is an ancestor of main, a no-op when main is an ancestor of
// agent (or hashes match), or a single merge commit when histories
// diverged. Hash rewriting NEVER happens here — that's the rebase
// fallback's job.
//
// Conflict resolution is StrategyLocalWins for steady-state sync: the
// agent's local edits win overlapping paths against main. This matches
// the design intent of the old replay-based reconcile.
//
// Holds rh.lockBranch(agentBranch) (inside mergeIntoBranch) for the duration.
package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
)

// reconcileAgentMerge merges local main into agentBranch. Wraps
// mergeIntoBranch and seeds the watermark on success so the rewind
// fallback always has a usable base to walk back to.
func (rh *repoHandler) reconcileAgentMerge(ctx context.Context, agentBranch string, strategy ConflictStrategy) (AgentReconcileResult, error) {
	res, err := rh.mergeIntoBranch(ctx, "main", agentBranch, strategy)
	if err != nil {
		return res, fmt.Errorf("reconcileAgentMerge: %w", err)
	}

	// Advance watermark to current local main. The watermark is only
	// consulted on the rewind path, but bootstrap paths seed it and
	// reconcileAgent has always kept it current — preserve that invariant
	// so a future rewind has a sensible base.
	mainRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName("main"))
	if err != nil {
		return res, fmt.Errorf("reconcileAgentMerge: read local main for watermark: %w", err)
	}
	if err := rh.writeAgentBase(agentBranch, mainRef.Hash()); err != nil {
		return res, fmt.Errorf("reconcileAgentMerge: write watermark: %w", err)
	}

	log.Info().
		Str("branch", agentBranch).
		Str("mode", res.Mode).
		Str("new_tip", shortRefHash(plumbingHash(res.NewTip))).
		Msg("reconcileAgentMerge: complete")
	return res, nil
}

// plumbingHash converts a hex string to plumbing.Hash, returning zero on
// empty input. Used in log statements where the NewTip may be empty for
// no-op results.
func plumbingHash(s string) plumbing.Hash {
	if s == "" {
		return plumbing.ZeroHash
	}
	return plumbing.NewHash(s)
}
