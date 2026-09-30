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
// upstreamMain is the consensus branch (whatever the origin names; never
// empty here, see Sync).
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
	originMainName := plumbing.NewRemoteReferenceName("origin", upstreamMain)
	originMainRef, err := rh.gits.Reference(originMainName)
	if err != nil {
		return MainReconcileResult{}, fmt.Errorf("reconcileMain: read origin/%s: %w", upstreamMain, err)
	}
	// F09: origin's main is trusted as fetched. A change is verified once,
	// at the gate that advanced main (CheckRange), never again here.
	return rh.reconcileMainTo(ctx, upstreamMain, originMainRef.Hash())
}

// reconcileMainTo moves the local upstream to originHash by fast-forward,
// create, or force-update. See reconcileMain.
func (rh *repoHandler) reconcileMainTo(ctx context.Context, upstreamMain string, originHash plumbing.Hash) (MainReconcileResult, error) {
	res, class, err := rh.advanceBranchTo(ctx, upstreamMain, originHash)
	if err != nil {
		return res, fmt.Errorf("reconcileMain: %w", err)
	}
	switch {
	case res.Mode == ModeFF && class.created:
		// Created at origin's tip: nothing to report beyond the result.
	case res.Mode == ModeFF:
		log.Info().Str("branch", upstreamMain).Str("to", originHash.String()[:8]).Msg("reconcileMain: fast-forward")
	case res.Mode == ModeRewound:
		logEv := log.Warn().
			Str("branch", upstreamMain).
			Str("local", class.from.String()[:8]).
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
	}
	return res, nil
}

// advanceMove says how advanceBranchTo moved a branch, for its callers' logs:
// the tip it moved from, whether it created the branch, and — on a rewind —
// how the old and new tips relate.
type advanceMove struct {
	rewindClassification
	from    plumbing.Hash
	created bool
}

// advanceBranchTo moves refs/heads/<branch> to hash by create, fast-forward or
// force-update, and brings every derived table along: the branches row,
// branch_commits (purged first on a force-update, since populateCommitLog only
// inserts), then notifyCommit (commit_log, index, observers). It is the body
// reconcileMain has always run, factored out so receive-pack registers a
// pushed branch through the SAME steps; it is name-neutral and logs nothing —
// each caller words its own log lines.
//
// The caller holds lockBranch(branch). Every step after SetReference is
// context-bound SQL, so a caller whose ctx can be cancelled (a request) must
// pass one that cannot: a cancellation between the ref move and notifyCommit
// is the torn state kb/invariants/store/notify-commit-single-chokepoint
// forbids.
func (rh *repoHandler) advanceBranchTo(ctx context.Context, branch string, hash plumbing.Hash) (MainReconcileResult, advanceMove, error) {
	refName := plumbing.NewBranchReferenceName(branch)
	if hash == plumbing.ZeroHash {
		return MainReconcileResult{Mode: ModeNoop}, advanceMove{}, nil // nothing to move to
	}
	localRef, err := rh.gits.Reference(refName)
	if err != nil {
		// The branch doesn't exist — create it at hash.
		if err := rh.deriveBeforeAdvance(ctx, hash); err != nil {
			return MainReconcileResult{}, advanceMove{}, err
		}
		if err := rh.gits.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
			return MainReconcileResult{}, advanceMove{}, fmt.Errorf("create local %s: %w", branch, err)
		}
		if _, err := rh.EnsureBranch(ctx, branch, "refs/heads/"+branch); err != nil {
			return MainReconcileResult{}, advanceMove{}, fmt.Errorf("ensure %s: %w", branch, err)
		}
		if err := rh.populateCommitLog(ctx, branch); err != nil {
			return MainReconcileResult{}, advanceMove{}, fmt.Errorf("populate commit_log after create: %w", err)
		}
		if err := rh.notifyCommit(ctx, branch, hash); err != nil {
			return MainReconcileResult{}, advanceMove{}, fmt.Errorf("notify after create: %w", err)
		}
		return MainReconcileResult{Mode: ModeFF, NewTip: hash.String()}, advanceMove{created: true}, nil
	}

	// Self-heal: a git ref can exist without a matching branches SQL row
	// (legacy state from older migrations, or a direct SetReference somewhere
	// that bypassed EnsureBranch). Without the SQL row, downstream readers
	// like mergeIntoBranchLocked.branchID error out with "branch not found",
	// which previously killed the entire reconcile loop. EnsureBranch is
	// idempotent (INSERT OR IGNORE + cache), so this is a cheap invariant
	// to assert on every success path.
	if _, err := rh.EnsureBranch(ctx, branch, "refs/heads/"+branch); err != nil {
		return MainReconcileResult{}, advanceMove{}, fmt.Errorf("ensure %s: %w", branch, err)
	}
	localHash := localRef.Hash()
	move := advanceMove{from: localHash}

	if localHash == hash {
		return MainReconcileResult{Mode: ModeNoop}, move, nil
	}

	localCommit, err := rh.repo.CommitObject(localHash)
	if err != nil {
		return MainReconcileResult{}, move, fmt.Errorf("local commit: %w", err)
	}
	newCommit, err := rh.repo.CommitObject(hash)
	if err != nil {
		return MainReconcileResult{}, move, fmt.Errorf("target commit: %w", err)
	}

	isLocalAncestor, err := localCommit.IsAncestor(newCommit)
	if err != nil {
		return MainReconcileResult{}, move, fmt.Errorf("IsAncestor: %w", err)
	}
	if isLocalAncestor {
		// Fast-forward.
		if err := rh.deriveBeforeAdvance(ctx, hash); err != nil {
			return MainReconcileResult{}, move, err
		}
		if err := rh.gits.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
			return MainReconcileResult{}, move, fmt.Errorf("fast-forward: %w", err)
		}
		if err := rh.populateCommitLog(ctx, branch); err != nil {
			return MainReconcileResult{}, move, fmt.Errorf("populate commit_log after fast-forward: %w", err)
		}
		if err := rh.notifyCommit(ctx, branch, hash); err != nil {
			return MainReconcileResult{}, move, fmt.Errorf("notify after fast-forward: %w", err)
		}
		return MainReconcileResult{Mode: ModeFF, NewTip: hash.String()}, move, nil
	}

	// hash is not a descendant of the local tip → rewind / divergent advance.
	// Classify the rewind so callers' logs can distinguish ordinary rewind,
	// disjoint history (replaced wholesale), and the rare case where
	// MergeBase itself fails (object-store IO error).
	move.rewindClassification = classifyMainRewind(localCommit, newCommit)

	if err := rh.deriveBeforeAdvance(ctx, hash); err != nil {
		return MainReconcileResult{}, move, err
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
		return MainReconcileResult{}, move, fmt.Errorf("force-update: %w", err)
	}
	// The old chain is no longer reachable from the branch. Purge stale
	// branch_commits rows before repopulating; otherwise Verify reports
	// unreachable rows because populateCommitLog only INSERTs.
	if err := rh.repopulateBranch(ctx, branch); err != nil {
		return MainReconcileResult{}, move, fmt.Errorf("repopulate commit_log after force-update: %w", err)
	}
	if err := rh.notifyCommit(ctx, branch, hash); err != nil {
		return MainReconcileResult{}, move, fmt.Errorf("notify after force-update: %w", err)
	}
	return MainReconcileResult{Mode: ModeRewound, NewTip: hash.String()}, move, nil
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
