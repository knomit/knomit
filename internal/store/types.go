package store

import "knomit/internal/store/merge"

// ReplayConfig controls replay behavior.
type ReplayConfig struct {
	Strategy          merge.ConflictStrategy
	AgentBranch       string
	DefaultBranch     string
	UseExistingBranch bool // if true and AgentBranch exists on target, replay on top of it
	OnProgress        func(current, total int)

	// SkipIndexSync suppresses the target store's per-commit search-index sync
	// for the duration of the replay. Set this ONLY when the caller runs a full
	// Rebuild on the target afterward (the origin-apply flow does, at commit
	// time) — it turns ~N redundant incremental syncs (the first a full rebuild
	// against an empty index) into a single Rebuild. The target's git tree and
	// commit_log are written normally; only the derived index tables are left
	// for Rebuild to reconstruct. Default false preserves today's behavior.
	SkipIndexSync bool
}

type ReplayResult struct {
	TotalFacts           int
	FromLocal            int
	FromRemote           int
	Overwrites           int
	RefsResolvedFromHist int
	DanglingRefsDropped  int
}
