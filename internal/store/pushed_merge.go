// Merging a peer's pushed branch into this instance's agent branch (F11 UI
// merge). A peer pushes its own agent branch to this host over /git (F11);
// here the host's operator merges it, from the web UI, into the host's own
// agent branch, and the host's ordinary sync carries it upstream.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ErrBranchMoved: the pushed branch no longer points at the commit the caller
// confirmed. Match with errors.Is; errors.As a *BranchMovedError for the two
// hashes.
var ErrBranchMoved = errors.New("branch moved since it was reviewed")

// ErrUnrelatedHistories: the two branches share no commit, so there is no
// merge base to merge from.
var ErrUnrelatedHistories = errors.New("unrelated histories")

// BranchMovedError is MergePushed's refusal when src is not at srcTip. Nothing
// was written: the caller shows the new tip and asks again.
type BranchMovedError struct {
	Branch   string
	Expected string
	Actual   string
}

func (e *BranchMovedError) Error() string {
	return fmt.Sprintf("%s: %s is at %s, not the reviewed %s", ErrBranchMoved.Error(), e.Branch, e.Actual, e.Expected)
}

func (e *BranchMovedError) Is(target error) bool { return target == ErrBranchMoved }

// MergePushed merges the pushed branch src, at exactly the commit srcTip, into
// dst (the caller passes this instance's agent branch), under dst's branch
// lock and through notifyCommit.
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
func (s *Service) MergePushed(ctx context.Context, src, dst string, srcTip plumbing.Hash, side ResolutionSide) (AgentReconcileResult, error) {
	if srcTip.IsZero() {
		return AgentReconcileResult{}, fmt.Errorf("MergePushed: the reviewed tip of %s is required", src)
	}
	switch side {
	case "", ResolveSrc, ResolveDst:
	default:
		return AgentReconcileResult{}, fmt.Errorf("MergePushed: unknown side %q", side)
	}
	rh := s.rh
	unlock := rh.lockBranch(dst)
	defer unlock()
	return rh.mergeIntoBranchLockedOpts(ctx, src, dst, StrategyRefuse, nil,
		mergeOpts{srcTip: srcTip, record: true, side: side})
}

// PushedBranchInfo is what the pushed-branches list shows for one branch.
type PushedBranchInfo struct {
	Name      string
	Tip       string
	TipTime   time.Time
	TipAuthor string
	// ToMerge counts commits reachable from Tip and not from the agent
	// branch: what a merge would bring in. 0 once merged.
	ToMerge int
	// MergeBase is the best common ancestor of Tip and the agent branch ("" if
	// the histories are unrelated): the `since` for the changes list.
	MergeBase string
	// OtherFilesChanged counts paths outside the ontology root that differ
	// between MergeBase (or nothing) and Tip — the changes list shows facts
	// only.
	OtherFilesChanged int
}

// PushedBranches reports each named branch against agentBranch. The caller
// decides which branches are pushed ones (repos.RepoInstance.IsPushedBranch);
// this only reads. The agent branch's history is walked once for all of them.
func (s *Service) PushedBranches(ctx context.Context, names []string, agentBranch, ontologyRoot string) ([]PushedBranchInfo, error) {
	rh := s.rh
	agentRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return nil, fmt.Errorf("pushed branches: agent branch %q: %w", agentBranch, err)
	}
	agentCommit, err := object.GetCommit(rh.gits, agentRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("pushed branches: agent tip: %w", err)
	}
	inAgent := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, agentRef.Hash(), nil, func(c *object.Commit) { inAgent[c.Hash] = true }); err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(ontologyRoot, "/") + "/"

	out := make([]PushedBranchInfo, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(name))
		if err != nil {
			return nil, fmt.Errorf("pushed branches: %q: %w", name, err)
		}
		tip, err := object.GetCommit(rh.gits, ref.Hash())
		if err != nil {
			return nil, fmt.Errorf("pushed branches: %q tip: %w", name, err)
		}
		info := PushedBranchInfo{
			Name: name, Tip: tip.Hash.String(),
			TipTime: tip.Committer.When, TipAuthor: tip.Author.Name,
		}
		if !inAgent[tip.Hash] {
			if err := walkHistory(rh.gits, tip.Hash, inAgent, func(*object.Commit) { info.ToMerge++ }); err != nil {
				return nil, err
			}
		}
		var baseTree *object.Tree
		if bases, err := tip.MergeBase(agentCommit); err == nil && len(bases) > 0 {
			info.MergeBase = bases[0].Hash.String()
			if baseTree, err = bases[0].Tree(); err != nil {
				return nil, err
			}
		}
		tipTree, err := tip.Tree()
		if err != nil {
			return nil, err
		}
		changes, err := object.DiffTree(baseTree, tipTree)
		if err != nil {
			return nil, fmt.Errorf("pushed branches: %q diff: %w", name, err)
		}
		for _, ch := range changes {
			p := ch.To.Name
			if p == "" {
				p = ch.From.Name
			}
			if !strings.HasPrefix(p, prefix) {
				info.OtherFilesChanged++
			}
		}
		out = append(out, info)
	}
	return out, nil
}
