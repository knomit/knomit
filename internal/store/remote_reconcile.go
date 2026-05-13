// Origin reconciliation primitives. The agent branch is reconciled by
// replaying its unpushed commits onto an upstream (origin/agent/<host>
// when present, else origin/main); local main is a strict mirror of
// origin/main. The reconcile primitives are the single source of truth
// for "what does sync do" — Sync/Push/InitFromRemote/ActivateSync all
// call into them.
package store

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// agentUpstream identifies the ref the agent branch should reconcile against.
type agentUpstream struct {
	refName plumbing.ReferenceName // remote-tracking ref name (refs/remotes/origin/...)
	hash    plumbing.Hash          // current tip of that ref
	// isOwnAgent is true when the upstream is origin/agent/<host>.
	// false when we fell back to origin/main (no remote agent branch yet).
	isOwnAgent bool
}

// resolveAgentUpstream returns the upstream the local agent branch should
// reconcile against: origin/agent/<host> when that ref exists locally
// (post-fetch), otherwise origin/main. Errors if neither ref exists.
func (rh *repoHandler) resolveAgentUpstream(ctx context.Context, agentBranch string) (agentUpstream, error) {
	agentRefName := plumbing.NewRemoteReferenceName("origin", agentBranch)
	if ref, err := rh.gits.Reference(agentRefName); err == nil {
		return agentUpstream{refName: agentRefName, hash: ref.Hash(), isOwnAgent: true}, nil
	}
	mainRefName := plumbing.NewRemoteReferenceName("origin", "main")
	if ref, err := rh.gits.Reference(mainRefName); err == nil {
		return agentUpstream{refName: mainRefName, hash: ref.Hash(), isOwnAgent: false}, nil
	}
	return agentUpstream{}, fmt.Errorf("resolveAgentUpstream: neither origin/%s nor origin/main present (fetch first?)", agentBranch)
}

// unpushedCommits returns commits reachable from localTip but not from
// upstreamTip, ordered oldest → newest (the order in which they should be
// replayed). The walk follows first-parent ancestry only — merge commits
// take their "ours" side, matching the linear-history goal.
//
// Returns disjoint=true when localTip and upstreamTip share no common
// ancestor: every local commit (back to root) is treated as unpushed.
// In that case the caller will replay the entire chain onto upstreamTip.
//
// Returns empty (and disjoint=false) when localTip is an ancestor of
// upstreamTip (nothing local to replay — caller will fast-forward).
func (rh *repoHandler) unpushedCommits(localTip, upstreamTip plumbing.Hash) ([]*object.Commit, bool, error) {
	if localTip == upstreamTip {
		return nil, false, nil
	}

	local, err := rh.repo.CommitObject(localTip)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: local commit %s: %w", localTip, err)
	}
	upstream, err := rh.repo.CommitObject(upstreamTip)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: upstream commit %s: %w", upstreamTip, err)
	}

	// If local is an ancestor of upstream, nothing to replay.
	isAncestor, err := local.IsAncestor(upstream)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: IsAncestor: %w", err)
	}
	if isAncestor {
		return nil, false, nil
	}

	bases, err := local.MergeBase(upstream)
	if err != nil {
		return nil, false, fmt.Errorf("unpushedCommits: MergeBase: %w", err)
	}

	var stopAt plumbing.Hash
	disjoint := false
	if len(bases) == 0 {
		disjoint = true
		// Walk all the way back to root.
		stopAt = plumbing.ZeroHash
	} else {
		stopAt = bases[0].Hash
	}

	// Walk first-parent from local back to (but not including) stopAt.
	var collected []*object.Commit
	cur := local
	for {
		if cur.Hash == stopAt {
			break
		}
		collected = append(collected, cur)
		if cur.NumParents() == 0 {
			break
		}
		parent, err := cur.Parents().Next()
		if err != nil {
			return nil, false, fmt.Errorf("unpushedCommits: walk first-parent at %s: %w", cur.Hash, err)
		}
		cur = parent
	}

	// Reverse to oldest-first.
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	return collected, disjoint, nil
}
