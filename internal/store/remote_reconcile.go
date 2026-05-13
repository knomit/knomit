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
