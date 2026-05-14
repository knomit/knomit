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
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
)

// rewindClassification distinguishes three outcomes when reconcileMain
// finds that origin/main is not an ancestor of local main. They MUST be
// kept separate because each produces a materially different operator
// signal:
//
//   - Disjoint=true, BaseErr=nil: confirmed disjoint history — origin
//     was replaced wholesale (unrelated repo, full rewrite, corruption).
//     The operator likely wants to investigate before letting sync continue.
//   - Disjoint=false, BaseErr=nil: shared ancestor exists — ordinary rewind
//     or force-push. The watermark/replay machinery handles it routinely.
//   - BaseErr != nil: MergeBase itself could not run (object-store IO
//     failure, etc.). Disjoint detection was UNRELIABLE; the operator
//     must see the error rather than receive the milder "not a descendant"
//     log line that would otherwise be emitted.
//
// Collapsing the third case into Disjoint=false (the original code) was
// the bug PR #61 review finding #3 flagged.
type rewindClassification struct {
	Disjoint bool
	BaseErr  error
}

// classifyMainRewind categorises the relationship between local main and
// origin/main once origin has been determined to be a non-ancestor. See
// rewindClassification for the three outcomes.
func classifyMainRewind(local, origin *object.Commit) rewindClassification {
	bases, err := local.MergeBase(origin)
	if err != nil {
		return rewindClassification{BaseErr: err}
	}
	return rewindClassification{Disjoint: len(bases) == 0}
}

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
//
// Mode values:
//   - ModeNoop:    local main was already at origin/main; no change.
//   - ModeFF:      local main fast-forwarded to origin/main.
//   - ModeRewound: origin/main was not a descendant of local main; local
//     main was force-updated. The caller routes the agent
//     branch to the rebase fallback.
type MainReconcileResult struct {
	Mode   Mode   `json:"mode"`
	NewTip string `json:"new_tip,omitempty"`
}

// reconcileMain updates the local consensus branch (upstreamMain) to track
// origin/<upstreamMain>. Fast-forwards when the origin ref is a descendant of
// local. When it is NOT a descendant (rewind, force-push, or disjoint history
// on the remote), force-updates the local branch and reports Mode=ModeRewound
// — the caller must then re-migrate the agent branch against the new
// upstream.
//
// upstreamMain selects the consensus branch (typically "main" but
// configurable to "master" or any other name). Empty defaults to "main".
//
// The disjoint-history sub-case (no MergeBase between local and origin)
// is detected and logged distinctly from a plain rewind; both still
// dispatch to the rebase fallback, but the operator gets a clear signal
// when the remote has been replaced wholesale (unrelated repo, corruption,
// or a complete history rewrite).
//
// Errors if origin/<upstreamMain> is not present locally (caller must fetch
// first).
//
// Caller must hold rh.lockBranch(upstreamMain). After every ref advance,
// commit_log is repopulated and the index manager is notified so
// downstream readers see consistent state.
func (rh *repoHandler) reconcileMain(ctx context.Context, upstreamMain string) (MainReconcileResult, error) {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	originMainName := plumbing.NewRemoteReferenceName("origin", upstreamMain)
	originMainRef, err := rh.gits.Reference(originMainName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: read origin/%s: %w", upstreamMain, err)
	}
	originHash := originMainRef.Hash()

	localMainName := plumbing.NewBranchReferenceName(upstreamMain)
	localMainRef, err := rh.gits.Reference(localMainName)
	if err != nil {
		// Local upstream branch doesn't exist — create at origin/<upstreamMain>.
		if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: create local %s: %w", upstreamMain, err)
		}
		if _, err := rh.EnsureBranch(ctx, upstreamMain, "refs/heads/"+upstreamMain); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: ensure %s: %w", upstreamMain, err)
		}
		if err := rh.populateCommitLog(ctx, upstreamMain); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: populate commit_log after create: %w", err)
		}
		if err := rh.notifyCommit(ctx, upstreamMain, originHash); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after create: %w", err)
		}
		return MainReconcileResult{Mode: ModeFF, NewTip: originHash.String()}, nil
	}
	localHash := localMainRef.Hash()

	if localHash == originHash {
		return MainReconcileResult{Mode: ModeNoop}, nil
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
		if err := rh.populateCommitLog(ctx, upstreamMain); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: populate commit_log after fast-forward: %w", err)
		}
		if err := rh.notifyCommit(ctx, upstreamMain, originHash); err != nil {
			return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after fast-forward: %w", err)
		}
		log.Info().Str("branch", upstreamMain).Str("to", originHash.String()[:8]).Msg("reconcileMain: fast-forward")
		return MainReconcileResult{Mode: ModeFF, NewTip: originHash.String()}, nil
	}

	// origin/main is not a descendant of local main → rewind / divergent advance.
	// Classify the rewind so the operator log distinguishes ordinary
	// rewind, disjoint history (origin replaced wholesale), and the rare
	// case where MergeBase itself fails (object-store IO error).
	class := classifyMainRewind(localCommit, originCommit)

	if err := rh.gits.SetReference(plumbing.NewHashReference(localMainName, originHash)); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: force-update: %w", err)
	}
	// The old chain is no longer reachable from the upstream branch. Purge
	// stale branch_commits rows before repopulating; otherwise Verify reports
	// unreachable rows because populateCommitLog only INSERTs.
	if err := rh.purgeBranchCommits(ctx, upstreamMain); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: purge branch_commits after rewind: %w", err)
	}
	if err := rh.populateCommitLog(ctx, upstreamMain); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: populate commit_log after force-update: %w", err)
	}
	if err := rh.notifyCommit(ctx, upstreamMain, originHash); err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: notify after force-update: %w", err)
	}
	logEv := log.Warn().
		Str("branch", upstreamMain).
		Str("local", localHash.String()[:8]).
		Str("origin", originHash.String()[:8])
	switch {
	case class.BaseErr != nil:
		logEv.Err(class.BaseErr).
			Msgf("reconcileMain: origin/%s force-updated; MergeBase classification failed (disjoint detection unreliable — investigate object store)", upstreamMain)
	case class.Disjoint:
		logEv.Bool("disjoint", true).
			Msgf("reconcileMain: origin/%s has DISJOINT history (no common ancestor); force-updated", upstreamMain)
	default:
		logEv.Bool("disjoint", false).
			Msgf("reconcileMain: origin/%s is not a descendant of local %s; force-updated", upstreamMain, upstreamMain)
	}
	return MainReconcileResult{Mode: ModeRewound, NewTip: originHash.String()}, nil
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

// reconcileAgent dispatches to either the merge-based steady-state path
// (reconcileAgentMerge) or the rebase fallback (reconcileAgentRebase)
// depending on whether reconcileMain detected a force-rewind on origin/main.
//
// Routing:
//   - mainRewound=false → reconcileAgentMerge (creates at most one merge
//     commit, no hash rewriting).
//   - mainRewound=true  → rebase fallback: walks agent's local-only commits
//     since the watermark and replays them onto the
//     disjoint new main. This is the only path where
//     hash rewriting happens, and it's necessary for
//     scrub semantics to work (G6).
//
// The watermark is updated to current local main on both paths so it
// always has a usable base for a future rewind. Both paths hold
// rh.lockBranch(agentBranch) for the entire body, including the
// watermark write.
func (rh *repoHandler) reconcileAgent(ctx context.Context, agentBranch, upstreamMain string, strategy ConflictStrategy, mainRewound bool) (AgentReconcileResult, error) {
	if upstreamMain == "" {
		upstreamMain = "main"
	}
	if mainRewound {
		return rh.reconcileAgentRebase(ctx, agentBranch, upstreamMain, strategy)
	}
	return rh.reconcileAgentMerge(ctx, agentBranch, upstreamMain, strategy)
}

// shortRefHash returns the first 8 chars of a ref hash for log output, or
// "<zero>" for the zero hash.
func shortRefHash(h plumbing.Hash) string {
	if h == plumbing.ZeroHash {
		return "<zero>"
	}
	return h.String()[:8]
}
