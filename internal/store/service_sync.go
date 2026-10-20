package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/store/internal/vcs"
	storesync "knomit/internal/store/sync"
)

// DropUnpublishedCommit is the operator's way out of a refusal by the judge
// of this instance's own commits (JudgeOwnRange): the refused commit stays
// on the agent branch, so every later move of main is refused too, and
// knomit never rewinds a branch by itself. It rebuilds branch on the
// consensus branch's tip, replaying in order every commit of branch ∖ main
// on the first-parent line (re-signed by this instance, its own content
// winning a conflict) EXCEPT drop, so nothing but drop's own change is lost:
//   - a merge commit is replayed as a merge of the same other parents, so
//     what it brought in stays (a member's branch the merger had merged, a
//     committed experiment);
//   - a merge whose other parents reach drop (it brought drop in, or its
//     side forked after drop) is unfolded: its side's own non-merge commits
//     are replayed one by one, parents first, drop excluded (counted in
//     mergesDropped). A conflict resolution the side's own merges made is
//     not kept. With ownOnly (the fleet, whose judge refuses a commit
//     re-signed for another agent) a side commit authored by another agent
//     refuses the drop (*DropForeignError) instead of re-signing it;
//   - where a later commit edited a file drop wrote, the later version is
//     kept, whole (#533 review S3).
//
// drop must be a non-merge commit of branch that main does not hold
// (ErrNotUnpublished, *DropMergeError). Both refusals that leave the
// operator stuck (*DropMergeError, *DropForeignError) carry what
// DropUnpublishedCommitLeavingOutMerges would lose instead. Returns how many
// commits were replayed and how many merges were unfolded.
func (s *Service) DropUnpublishedCommit(ctx context.Context, branch string, drop plumbing.Hash, ownOnly bool) (replayed, mergesDropped int, err error) {
	out, err := s.dropUnpublished(ctx, branch, drop, ownOnly, false, nil)
	return out.Replayed, out.Unfolded, err
}

// DropUnpublishedCommitLeavingOutMerges is the CONFIRMED fallback (#533
// review 3 SF1) for when DropUnpublishedCommit refuses because nothing but
// the named commit can be lost and that cannot be kept (a merge as the
// target; another agent's commit an unfold would re-sign, its author gone
// for good): it rebuilds branch on the consensus branch's tip as
// DropUnpublishedCommit does, except that drop, when it is on the
// first-parent line, and every merge on that line whose other parents reach
// drop are left out WHOLE, with every commit their sides brought (no
// unfold, so nothing is re-signed for another agent). Every other commit and
// merge is replayed as DropUnpublishedCommit replays it. Returns how many
// commits were replayed and what was left out (the merges and their sides'
// commits, as the refusal listed them). The caller asks the operator first:
// the left-out work is gone from this branch. confirmed is the list of
// commit hashes the operator was shown and confirmed; when what would be
// left out now differs (the branch moved since), nothing changes and the
// answer is a *LeaveOutChangedError carrying the new list to confirm.
func (s *Service) DropUnpublishedCommitLeavingOutMerges(ctx context.Context, branch string, drop plumbing.Hash, confirmed []string) (replayed int, leftOut []LostCommit, err error) {
	out, err := s.dropUnpublished(ctx, branch, drop, false, true, confirmed)
	return out.Replayed, out.LeftOut, err
}

func (s *Service) dropUnpublished(ctx context.Context, branch string, drop plumbing.Hash, ownOnly, leaveOut bool, confirmed []string) (storesync.DropOutcome, error) {
	upstream := s.UpstreamBranch()
	if upstream == "" || branch == "" || branch == upstream {
		return storesync.DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %q is not a branch to rebuild on %q", branch, upstream)
	}
	return s.rh.DropUnpublished(ctx, upstream, branch, drop, ownOnly, leaveOut, confirmed)
}

// SetReplaySkipPaths names the paths a rewind replay never carries: a
// non-merge commit of this branch that changes nothing but these paths is
// dropped from the replay instead of being re-submitted onto the new
// upstream. The fleet repository sets its master marker
// (fact.FleetMasterPath): a copy that is not the master on the new upstream
// never re-submits its own claim to the role, or its stand-down, which the
// host's judge would refuse for good (user ruling 2026-10-16: the loser
// drops its own claim). Any other repository sets nothing.
func (s *Service) SetReplaySkipPaths(paths ...string) {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	s.rh.SetReplaySkip(set)
}

// MainHistory reads the consensus branch's tip and its history.
func (s *Service) MainHistory(ctx context.Context) (*MainHistory, error) {
	upstream := s.UpstreamBranch()
	if upstream == "" {
		return nil, fmt.Errorf("MainHistory: %w", ErrNoConsensusBranch)
	}
	uh, err := s.rh.ResolveRef(ctx, upstream)
	if err != nil {
		return nil, fmt.Errorf("MainHistory: %s: %w", upstream, err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := vcs.WalkHistory(s.rh.Storer(), uh, nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return nil, fmt.Errorf("MainHistory: %w", err)
	}
	return storesync.NewMainHistory(uh, onMain), nil
}

// AdmitPeerBranch is where a fetched member branch enters this repository.
// It judges tip with JudgePeerBranch and, admitted, promotes it.
//
// Admitted: refs/heads/<branch> is set to tip as receive-pack's register sets
// it (a new branch seeded from the consensus branch, then moved; a rewind is
// allowed, as on a push), through the commit chokepoint, which kicks the
// consensus merger. Returns "" then. Refused: nothing moves, and the refusal
// text is returned. err is for what could not be judged at all.
//
// main is the consensus branch's history (MainHistory); nil reads it here.
// The caller decides who may call this (only the fleet's master); the store
// checks only the branch's shape: an agent/* branch that is neither the
// consensus branch nor own.
func (s *Service) AdmitPeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash, main *MainHistory) (refusal string, err error) {
	refusal, err = s.JudgePeerBranch(ctx, branch, own, tip, main)
	if err != nil || refusal != "" {
		return refusal, err
	}
	return "", s.rh.PromotePeer(ctx, branch, s.UpstreamBranch(), tip)
}

// JudgePeerBranch judges a member branch's tip without moving anything: the
// commits of tip ∖ main (EVERY one, not the tip alone), with receive-pack's
// core: the private-path closure, then judgeIncoming against the fleet at
// the consensus branch — which, the caller being the fleet repository's
// master, is this repository itself. There is no pusher, so there is no
// signer = pusher rule; every other rule (signature, records, rules A/B,
// artifact rules) is the push's. Returns the refusal text, or "".
//
// It is the fetch judge (AdmitPeerBranch) and the master's re-judge of a
// local agent/* branch when main has moved since it was admitted (the
// merger's, on both hostings: a revocation on main can turn an admitted,
// unmerged tip into a refused one).
func (s *Service) JudgePeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash, main *MainHistory) (string, error) {
	rh := s.rh
	upstream := s.UpstreamBranch()
	switch {
	case upstream == "":
		return "", fmt.Errorf("JudgePeerBranch: %w", ErrNoConsensusBranch)
	case !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own:
		return "", fmt.Errorf("JudgePeerBranch: %q is not a member's agent branch", branch)
	case tip.IsZero():
		return "", fmt.Errorf("JudgePeerBranch: the tip of %s is required", branch)
	}
	if main == nil {
		var err error
		if main, err = s.MainHistory(ctx); err != nil {
			return "", err
		}
	}
	var commits []*object.Commit
	if err := vcs.WalkHistory(rh.Storer(), tip, main.OnMain(), func(c *object.Commit) { commits = append(commits, c) }); err != nil {
		return "", fmt.Errorf("JudgePeerBranch: %w", err)
	}
	if msg := rh.privatePathClosure(ctx, rh.Storer(), upstream, commits); msg != "" {
		return msg, nil
	}
	lg := log.With().Str("repo", rh.Name()).Str("branch", branch).Logger()
	self := func(context.Context) (Fleet, error) { return s.FleetAt(upstream) }
	return rh.judgeIncoming(ctx, upstream, self, nil, tip, commits, lg), nil
}

// DemotePeerBranch removes the local refs/heads/<branch> when it is still at
// tip: the fleet master's answer to a re-judge that refused a branch it had
// admitted (PR #524 N3), so the merger cannot merge it. A branch that moved
// meanwhile (a push or a promotion of a newer tip) is left alone, and
// reported false. Only an agent/* branch other than the consensus branch and
// own is ever touched. The objects stay; on the fleet repository a held
// commit no admitted ref reaches is judged again by receive-pack (heldAdmitted).
func (s *Service) DemotePeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash) (bool, error) {
	rh := s.rh
	upstream := s.UpstreamBranch()
	if !strings.HasPrefix(branch, "agent/") || branch == upstream || branch == own {
		return false, fmt.Errorf("DemotePeerBranch: %q is not a member's agent branch", branch)
	}
	id, err := rh.BranchID(ctx, branch)
	if err != nil {
		return false, nil // not a registered branch: nothing to demote
	}
	defer rh.LockBranch(branch)()
	ref, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(branch))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("DemotePeerBranch: read %s: %w", branch, err)
	}
	if ref.Hash() != tip {
		return false, nil
	}
	if err := rh.DropBranchLocked(context.WithoutCancel(ctx), branch, id); err != nil {
		return false, fmt.Errorf("DemotePeerBranch: %w", err)
	}
	return true, nil
}

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
// fetch creates it.
func (s *Service) EnsureLocalUpstream(ctx context.Context, upstream string) (bool, error) {
	if upstream == "" {
		return false, nil
	}
	rh := s.rh
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
// (upstream IS the agent branch) is a no-op here for the same reason.
func (s *Service) AdvanceLocalUpstream(ctx context.Context, agentBranch, upstream string) (MainReconcileResult, error) {
	if agentBranch == "" || upstream == "" || agentBranch == upstream {
		return MainReconcileResult{Mode: ModeNoop}, nil
	}
	rh := s.rh
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
		return MainReconcileResult{Mode: ModeNoop}, nil
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
		return MainReconcileResult{Mode: ModeNoop}, nil
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
	return MainReconcileResult{Mode: ModeFF, NewTip: agentRef.Hash().String()}, nil
}
