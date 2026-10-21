package repos

import (
	"strings"

	"knomit/internal/store"
)

// IsPushedBranch reports whether branch is one a PEER pushed to this host
// (F11) and may therefore be merged into this instance's agent branch: an
// `agent/*` branch that is neither this instance's agent branch nor the
// repo's upstream.
//
// "This instance's agent branch" is ri.agentBranch, the branch persisted in
// control.db instance_identity, compared by EXACT equality. Nothing is
// inferred from the shape of a name (D18): a peer branch whose name happens
// to end in this instance's fp8 is still a peer's branch.
//
// It classifies by NAME, which kb/invariants/store/branch-roles warns
// against for roles that reconcile acts on destructively. Two things act on
// this answer, and neither is destructive: a human-started, conflict-refusing
// merge from the web UI, and — with `consensus: auto` on the host's consensus
// branch, or on the fleet repository at its master — the consensus merger
// (consensus.go), which makes the same
// conflict-refusing merge into this instance's own branch without a human.
// Neither ever writes the pushed branch. A subscription has no agent branch
// and no pushed branches.
func (ri *RepoInstance) IsPushedBranch(branch string) bool {
	upstream := ""
	_ = ri.WithRead(func(svc *store.Service) { upstream = svc.Remote().UpstreamBranch() })
	return ri.isPushedBranch(branch, upstream)
}

// isPushedBranch is IsPushedBranch with the upstream already read, for a
// caller that holds the store (the consensus merger lists every branch).
func (ri *RepoInstance) isPushedBranch(branch, upstream string) bool {
	if ri.subscribed || ri.agentBranch == "" || branch == ri.agentBranch {
		return false
	}
	if !strings.HasPrefix(branch, "agent/") {
		return false
	}
	return branch != upstream
}
