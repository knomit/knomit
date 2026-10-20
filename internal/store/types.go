package store

const ()

const (
// The `conflicts` setting's strategies have no constant: each is
// conflictsPolicy.strategy(), the value "conflicts:<facts>/<state>"
// (e.g. "conflicts:merge/consensus"). They run on the refusing walk as
// per-path resolutions; what a key set to off settles gets the calling
// site's own side-picking strategy (mergeOpts.factFallback). Chosen by
// the sites whose repo sets `conflicts` (or `consensus: auto`) at its
// consensus branch tip. See conflict_merge.go.
)

// ReplayConfig controls replay behavior.
type ReplayConfig struct {
	Strategy          ConflictStrategy
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

// FactIter is implemented by FactsIter in this package.
type FactIter interface {
	Next() (*FactRow, error)
	Close() error
}
