package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/store/remotesync"
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

func (s *Service) dropUnpublished(ctx context.Context, branch string, drop plumbing.Hash, ownOnly, leaveOut bool, confirmed []string) (remotesync.DropOutcome, error) {
	upstream := s.UpstreamBranch()
	if upstream == "" || branch == "" || branch == upstream {
		return remotesync.DropOutcome{}, fmt.Errorf("DropUnpublishedCommit: %q is not a branch to rebuild on %q", branch, upstream)
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
	return s.rh.ReadMainHistory(ctx, upstream)
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
	upstream := s.UpstreamBranch()
	if upstream == "" {
		return "", fmt.Errorf("JudgePeerBranch: %w", ErrNoConsensusBranch)
	}
	return s.rh.JudgePeerBranch(ctx, upstream, branch, own, tip, main)
}

// DemotePeerBranch removes the local refs/heads/<branch> when it is still at
// tip: the fleet master's answer to a re-judge that refused a branch it had
// admitted (PR #524 N3), so the merger cannot merge it. A branch that moved
// meanwhile (a push or a promotion of a newer tip) is left alone, and
// reported false. Only an agent/* branch other than the consensus branch and
// own is ever touched. The objects stay; on the fleet repository a held
// commit no admitted ref reaches is judged again by receive-pack (heldAdmitted).
func (s *Service) DemotePeerBranch(ctx context.Context, branch, own string, tip plumbing.Hash) (bool, error) {
	return s.rh.DemotePeerBranch(ctx, s.UpstreamBranch(), branch, own, tip)
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
	return s.rh.EnsureLocalUpstream(ctx, upstream)
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
	return s.rh.AdvanceLocalUpstream(ctx, agentBranch, upstream)
}
