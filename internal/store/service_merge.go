package store

import (
	"context"
	"fmt"

	"knomit/internal/fact"
	"knomit/internal/store/merge"

	"github.com/go-git/go-git/v5/plumbing"
)

// MergePushed merges the pushed branch src, at exactly the commit srcTip, into
// dst (the caller passes this instance's agent branch), under dst's branch
// lock and through NotifyCommit.
//
// It ALWAYS writes one merge commit [dst tip, srcTip] signed by this
// instance — never a fast-forward, never a tree-identical no-op — so the
// host's history records every merge a human made, and the peer tip becomes
// reachable from dst (which is what clears the list's to_merge). The only
// no-op is "srcTip is already in dst". It never writes src.
//
// Conflicts are refused (StrategyRefuse, *MergeConflictError) unless side is
// set, in which case every conflicting path takes that side: ResolveDst keeps
// the host's version, ResolveSrc takes the peer's. The conflict set is
// detected under the dst lock, so it is the set as of the merge itself.
//
// With no side and a `conflicts` setting at the consensus branch's tip (or
// its `consensus: auto` default), each conflicting path is settled as the
// setting says — the host's own branch is the consensus side — and only what a
// key set to off leaves is refused. An explicit side is a human's whole-set
// choice and is never overridden by the setting.
//
// A merge whose RESULT would change any private path of dst (.knomit/, any
// dot segment) is refused with *PrivatePathMergeError, whatever side or
// setting settled the conflicts (guardPrivate); one whose result adds or
// changes a fact that fails the ontology's validations at dst's tip is
// refused with *ValidationRefusedError (validateFacts, RuleValidation).
func (s *Service) MergePushed(ctx context.Context, src, dst string, srcTip plumbing.Hash, side merge.ResolutionSide) (merge.AgentReconcileResult, error) {
	if srcTip.IsZero() {
		return merge.AgentReconcileResult{}, fmt.Errorf("MergePushed: the reviewed tip of %s is required", src)
	}
	switch side {
	case "", merge.ResolveSrc, merge.ResolveDst:
	default:
		return merge.AgentReconcileResult{}, fmt.Errorf("MergePushed: unknown side %q", side)
	}
	rh := s.rh
	unlock := rh.LockBranch(dst)
	defer unlock()
	o := merge.MergeOpts{SrcTip: srcTip, Record: true, Side: side, GuardPrivate: true, ValidateFacts: true}
	strategy := merge.StrategyRefuse
	if side == "" {
		strategy = s.hostConflictStrategy(ctx, &o)
	}
	return rh.MergeIntoBranchLockedOpts(ctx, src, dst, strategy, nil, o)
}

// hostConflictStrategy is the strategy a host merge of a peer's branch runs
// with: StrategyRefuse, or — when the ontology at the tip of this repo's
// consensus branch (UpstreamBranch, never a hardcoded name) sets `conflicts`
// — that setting, with the host's own branch (dst, which the consensus branch
// follows) as the consensus side and Refuse for whatever a key set to off
// leaves.
func (s *Service) hostConflictStrategy(ctx context.Context, o *merge.MergeOpts) merge.ConflictStrategy {
	strategy, on := s.rh.ConflictsStrategy(ctx, s.ri.UpstreamBranch())
	if !on {
		return merge.StrategyRefuse
	}
	o.FactConsensus, o.FactFallback = fact.MergeDst, merge.StrategyRefuse
	return strategy
}

// MergeConsensus is MergePushed as the consensus merger (F08, `consensus:
// auto`) calls it: dst's lock, StrategyRefuse with no side (a conflict is
// refused and left for a human), and a host-signed merge commit [dst, srcTip]
// — plus ONE more no-op than MergePushed: when the merge would leave dst's
// tree unchanged AND every commit srcTip brings is a merge commit. That is a
// peer that only merged this host's history back into its branch, and
// recording it would hand the peer a new host commit to merge back on its next
// round: a merge commit per round, forever, between two idle instances.
//
// A peer's authored commit is never skipped this way, even when its content
// is already here: it is recorded like MergePushed records it.
//
// A `conflicts` setting at the consensus branch's tip — under `consensus:
// auto` it defaults to facts: merge / state: consensus — settles a conflict
// exactly as for MergePushed with no side; what a key set to off leaves is
// still refused and left for a human.
//
// Like MergePushed, a merge whose result would change a private path of dst
// is refused (*PrivatePathMergeError); the consensus merger remembers the
// refused tip and retries the branch once it moves.
func (s *Service) MergeConsensus(ctx context.Context, src, dst string, srcTip plumbing.Hash) (merge.AgentReconcileResult, error) {
	if srcTip.IsZero() {
		return merge.AgentReconcileResult{}, fmt.Errorf("MergeConsensus: the tip of %s is required", src)
	}
	rh := s.rh
	unlock := rh.LockBranch(dst)
	defer unlock()
	o := merge.MergeOpts{SrcTip: srcTip, Record: true, SkipMergeOnly: true, GuardPrivate: true, ValidateFacts: true}
	strategy := s.hostConflictStrategy(ctx, &o)
	return rh.MergeIntoBranchLockedOpts(ctx, src, dst, strategy, nil, o)
}
