// Main-branch reconcile, the watermark, and shared helpers. The rebase
// machinery lives in remote_reconcile_rebase.go; the merge machinery
// lives in branch_merge.go (mergeIntoBranch). reconcileAgent (the
// dispatcher) lives at the bottom of this file and routes to either
// rebase or merge based on whether origin/main was force-rewound this
// tick.
//
// The watermark (refs/knomit/agent-base/<branch>) tracks the main commit
// the agent last consumed. Required by the rebase fallback; advanced by
// every successful reconcileAgent regardless of which path ran.
//
// The reconcile primitives are the single source of truth for "what does
// sync do" — Sync/Push/InitFromRemote/ActivateSync all call into them.
package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
)

// agentBaseRefName returns the watermark ref name for an agent branch.
// The watermark lives under refs/knomit/ to keep it out of the regular
// refs/heads/ branch listing (and out of any "show me the branches" UI).
func agentBaseRefName(agentBranch string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/knomit/agent-base/" + agentBranch)
}

// readAgentBase returns the hash recorded in the watermark for agentBranch.
// Returns plumbing.ErrReferenceNotFound (wrapped) when the watermark has
// never been written for this branch.
func (rh *repoHandler) readAgentBase(agentBranch string) (plumbing.Hash, error) {
	ref, err := rh.gits.Reference(agentBaseRefName(agentBranch))
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// writeAgentBase updates the watermark for agentBranch to hash. The
// watermark identifies "the main commit this agent last consumed" and
// is read on the next Sync tick as the base for unpushedCommits.
func (rh *repoHandler) writeAgentBase(agentBranch string, hash plumbing.Hash) error {
	return rh.gits.SetReference(plumbing.NewHashReference(agentBaseRefName(agentBranch), hash))
}

// MainReconcileResult reports the outcome of reconcileMain.
type MainReconcileResult struct {
	FastForward bool   // local main was advanced to origin/main
	Rewound     bool   // origin/main was not a descendant of local main — force-updated
	NewTip      string // hash of the new local main tip (empty when no-op)
}

// reconcileMain updates local main to track origin/main. Fast-forwards
// when origin/main is a descendant of local main. When origin/main is
// NOT a descendant (rewind, force-push, or disjoint history on the
// remote), force-updates local main and reports Rewound=true — the
// caller must then re-migrate the agent branch against the new main.
//
// Errors if origin/main is not present locally (caller must fetch first).
//
// Caller must hold rh.lockBranch("main"). After every ref advance,
// commit_log is repopulated and the index manager is notified so
// downstream readers see consistent state.
func (rh *repoHandler) reconcileMain(ctx context.Context) (MainReconcileResult, error) {
	originMainName := plumbing.NewRemoteReferenceName("origin", "main")
	originMainRef, err := rh.gits.Reference(originMainName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: read origin/main: %w", err)
	}
	originHash := originMainRef.Hash()

	localMainName := plumbing.NewBranchReferenceName("main")
	localMainRef, err := rh.gits.Reference(localMainName)
	if err != nil {
		// Local main doesn't exist — create at origin/main.
		if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: create local main: %w", err)
		}
		if _, err := rh.EnsureBranch(ctx, "main", "refs/heads/main"); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: ensure main: %w", err)
		}
		if err := rh.populateCommitLog(ctx, "main"); err != nil {
			log.Warn().Err(err).Msg("reconcileMain: populate commit_log after create")
		}
		if err := rh.notifyCommit(ctx, "main", originHash); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after create: %w", err)
		}
		return MainReconcileResult{FastForward: true, NewTip: originHash.String()}, nil
	}
	localHash := localMainRef.Hash()

	if localHash == originHash {
		return MainReconcileResult{NewTip: originHash.String()}, nil
	}

	localCommit, err := rh.repo.CommitObject(localHash)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: local commit: %w", err)
	}
	originCommit, err := rh.repo.CommitObject(originHash)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: origin commit: %w", err)
	}

	isLocalAncestor, err := localCommit.IsAncestor(originCommit)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: IsAncestor: %w", err)
	}
	if isLocalAncestor {
		// Fast-forward.
		if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: fast-forward: %w", err)
		}
		if err := rh.populateCommitLog(ctx, "main"); err != nil {
			log.Warn().Err(err).Msg("reconcileMain: populate commit_log after fast-forward")
		}
		if err := rh.notifyCommit(ctx, "main", originHash); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after fast-forward: %w", err)
		}
		log.Info().Str("to", originHash.String()[:8]).Msg("reconcileMain: fast-forward")
		return MainReconcileResult{FastForward: true, NewTip: originHash.String()}, nil
	}

	// origin/main is not a descendant of local main → rewind / divergent advance.
	// Force-update local main; caller is responsible for re-migrating the agent.
	if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: force-update: %w", err)
	}
	// The old chain is no longer reachable from main. Purge stale
	// branch_commits rows before repopulating; otherwise Verify reports
	// unreachable rows because populateCommitLog only INSERTs.
	if err := rh.purgeBranchCommits(ctx, "main"); err != nil {
		log.Warn().Err(err).Msg("reconcileMain: purge branch_commits after rewind")
	}
	if err := rh.populateCommitLog(ctx, "main"); err != nil {
		log.Warn().Err(err).Msg("reconcileMain: populate commit_log after force-update")
	}
	if err := rh.notifyCommit(ctx, "main", originHash); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after force-update: %w", err)
	}
	log.Warn().
		Str("local", localHash.String()[:8]).
		Str("origin", originHash.String()[:8]).
		Msg("reconcileMain: origin/main is not a descendant of local main; force-updated")
	return MainReconcileResult{Rewound: true, NewTip: originHash.String()}, nil
}

// purgeBranchCommits deletes every branch_commits row for the given branch.
// Used by reconcileMain on a rewind so populateCommitLog can repopulate from
// the new HEAD without leaving stranded rows for commits that are no longer
// reachable.
func (rh *repoHandler) purgeBranchCommits(ctx context.Context, branch string) error {
	id, err := rh.branchID(ctx, branch)
	if err != nil {
		return fmt.Errorf("purgeBranchCommits: branchID: %w", err)
	}
	if _, err := rh.db.ExecContext(ctx, `DELETE FROM branch_commits WHERE branch_id = ?`, id); err != nil {
		return fmt.Errorf("purgeBranchCommits: delete: %w", err)
	}
	return nil
}

// reconcileAgent reconciles agentBranch by replaying its unpushed commits
// (those since the watermark) onto current local main. Conflict resolution
// uses the supplied strategy (agent-facing semantics — see replayCommit).
//
// The "upstream" for the agent is now local main (which reconcileMain has
// already aligned to origin/main). origin/agent/<host> is a push target
// only; the agent never reads from it after bootstrap.
//
// A per-branch watermark (refs/knomit/agent-base/<branch>) records the
// main commit the agent last consumed. unpushedCommits uses the
// watermark as its base, so:
//
//   - Forward main advances merge cleanly into the agent (the watermark
//     stays behind main; commits before main and after the watermark
//     don't exist on the agent, so the agent fast-forwards or replays
//     local-only commits on top of the new main).
//   - Forward main deletions correctly drop files from the agent (the
//     fast-forward picks up the deletion).
//   - Main force-push rewinds drop scrubbed files from the agent: when
//     the watermark equals the old main and the agent has no local
//     commits since it, unpushedCommits returns empty and the agent
//     fast-forwards onto the new main.
//
// If the watermark is missing or unreadable (first reconcile after
// bootstrap, or transient corruption), we fall back to MergeBase via a
// zero explicit base — the legacy behavior. This is defensive; InitRepo
// and InitFromRemote both seed the watermark, so the missing case
// shouldn't be hit in steady state.
//
// On a successful reconcile, the watermark is advanced to current
// local main. Holds rh.lockBranch(agentBranch) for the duration.
func (rh *repoHandler) reconcileAgent(ctx context.Context, agentBranch string, strategy ConflictStrategy) (AgentReconcileResult, error) {
	unlock := rh.lockBranch(agentBranch)
	defer unlock()

	mainRefName := plumbing.NewBranchReferenceName("main")
	mainRef, err := rh.gits.Reference(mainRefName)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgent: read local main: %w", err)
	}
	mainHash := mainRef.Hash()

	base, err := rh.readAgentBase(agentBranch)
	if err != nil {
		// Watermark missing — fall back to MergeBase by passing ZeroHash.
		// This handles older repos that predate the watermark and any
		// transient ref corruption. Steady-state init paths seed it.
		log.Warn().
			Str("branch", agentBranch).
			Err(err).
			Msg("reconcileAgent: watermark missing; falling back to MergeBase")
		base = plumbing.ZeroHash
	}

	log.Info().
		Str("branch", agentBranch).
		Str("upstream", "refs/heads/main").
		Str("base", shortRefHash(base)).
		Msg("reconcileAgent: replaying onto local main with watermark base")

	res, err := rh.replayOntoUpstream(ctx, agentBranch, mainHash, base, strategy)
	if err != nil {
		return res, err
	}

	// Advance the watermark to current local main. This is the commit the
	// agent has now "consumed" — the next Sync tick's unpushedCommits walk
	// will use it as its stop point.
	if err := rh.writeAgentBase(agentBranch, mainHash); err != nil {
		return res, fmt.Errorf("reconcileAgent: write watermark: %w", err)
	}
	return res, nil
}

// shortRefHash returns the first 8 chars of a ref hash for log output, or
// "<zero>" for the zero hash.
func shortRefHash(h plumbing.Hash) string {
	if h == plumbing.ZeroHash {
		return "<zero>"
	}
	return h.String()[:8]
}
