package repos

import (
	"strings"

	"knomit/internal/store"
)

// IsPushedBranch reports whether branch is one a PEER pushed to this host
// (F11) and may therefore be merged into this instance's agent branch from
// the web UI: an `agent/*` branch that is neither this instance's agent
// branch, nor the repo's upstream, nor this instance's own branch from before
// a hostname change (same `-<fp8>` suffix as the current agent branch).
//
// It classifies by NAME, which kb/invariants/store/branch-roles warns
// against for roles that reconcile acts on destructively. Nothing automatic
// acts on this answer: it only decides which branches the list shows and a
// human-started, conflict-refusing merge into this instance's own branch may
// take as its source. A subscription has no agent branch and no pushed
// branches.
func (ri *RepoInstance) IsPushedBranch(branch string) bool {
	if ri.subscribed || ri.agentBranch == "" || branch == ri.agentBranch {
		return false
	}
	if !strings.HasPrefix(branch, "agent/") {
		return false
	}
	if own := fp8Suffix(ri.agentBranch); own != "" && fp8Suffix(branch) == own {
		return false
	}
	upstream := ""
	_ = ri.WithRead(func(svc *store.Service) { upstream = svc.UpstreamBranch() })
	return branch != upstream
}

// fp8Suffix is the trailing `-<8 hex>` of an agent branch name
// (agent/<host>-<fp8>), or "" when the name has none.
func fp8Suffix(branch string) string {
	i := strings.LastIndexByte(branch, '-')
	if i < 0 || len(branch)-i-1 != 8 {
		return ""
	}
	s := branch[i+1:]
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ""
		}
	}
	return s
}
