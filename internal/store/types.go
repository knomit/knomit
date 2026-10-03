package store

type DirEntry struct {
	Name  string
	IsDir bool // true = subdirectory, false = .md file
}

type LogEntry struct {
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

type FileCounts struct {
	Added    int `json:"added,omitempty"`
	Modified int `json:"modified,omitempty"`
	Deleted  int `json:"deleted,omitempty"`
}

// CommitAuthor is the git author identity of a commit, recorded verbatim:
// Name is the agent-id (or human name on a merge); Email carries the
// +operation subaddress for agent writes. Distinct from the committer, which
// drops the operation tag (agents) or is GitHub itself (PR merges).
type CommitAuthor struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type LogEntryWithTags struct {
	Commit    string       `json:"commit"`
	Date      string       `json:"date"`
	Message   string       `json:"message"`
	Operation string       `json:"operation,omitempty"`
	Author    CommitAuthor `json:"author"`
	Files     FileCounts   `json:"files,omitempty"`
}

type ChangedFile struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "added", "modified", "deleted"
}

type CommitDetailResult struct {
	Commit    string        `json:"commit"`
	Date      string        `json:"date"`
	Message   string        `json:"message"`
	Operation string        `json:"operation,omitempty"`
	Author    CommitAuthor  `json:"author"`
	Files     []ChangedFile `json:"files"`
}

type ActivityResult struct {
	LastCommit string `json:"last_commit"` // ISO-8601 timestamp of most recent commit, or ""
	Total      int    `json:"total"`       // total commits touching this path
	Changes7d  int    `json:"changes_7d"`
	Changes30d int    `json:"changes_30d"`
	Changes90d int    `json:"changes_90d"`
}

// Mode classifies the outcome of a reconcile step. The same vocabulary is
// used by both MainReconcileResult (main side) and AgentReconcileResult
// (agent side); each side's doc lists which modes it can return.
type Mode string

const (
	ModeNoop    Mode = "noop"
	ModeFF      Mode = "ff"
	ModeMerge   Mode = "merge"
	ModeRebase  Mode = "rebase"
	ModeRewound Mode = "rewound"
)

// AgentReconcileResult reports the outcome of reconcileAgent.
//
// Mode values:
//   - ModeNoop:   agent ref unchanged.
//   - ModeFF:     agent fast-forwarded to local main (no new commit synthesized).
//   - ModeMerge:  one merge commit synthesized (steady-state path).
//   - ModeRebase: rebase-fallback path ran (origin/main rewind only).
//
// NumReplayed is populated only when Mode == ModeRebase.
//
// Settled is every conflicting path a three-way merge settled instead of
// refusing (a `conflicts` setting, a whole-set side, a caller's side
// resolution), read back from the Knomit-Merge / Knomit-Conflict lines the
// merge commit carries, so the result and the commit cannot disagree.
type AgentReconcileResult struct {
	Mode        Mode          `json:"mode"`
	NumReplayed int           `json:"num_replayed,omitempty"`
	NewTip      string        `json:"new_tip,omitempty"`
	Settled     []SettledPath `json:"settled,omitempty"`
}

// SettledPath is how one conflicting path was settled.
//
//   - Kept is "merged" (the two versions were field-merged), or "src" / "dst"
//     (that side's version landed — its deletion included).
//   - Dropped is the losing change as the trailer names it,
//     "<src|dst>-<modify|delete|add>", or "" for a field merge.
//   - Deleted is true when what landed is the path's ABSENCE: the kept side
//     had deleted it.
//   - Chosen is true when the caller resolved the path itself.
type SettledPath struct {
	Path    string `json:"path"`
	Kept    string `json:"kept"`
	Dropped string `json:"dropped,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Chosen  bool   `json:"chosen,omitempty"`
}

// SyncResult is the bundled outcome of a sync tick — one reconcile cycle
// that brings local main to origin/main (Main) and reconciles the agent
// branch (Agent) via merge or rebase fallback.
type SyncResult struct {
	Main  MainReconcileResult  `json:"main"`
	Agent AgentReconcileResult `json:"agent"`
}

// PushResult is returned by Push to report what happened.
type PushResult struct {
	Pushed bool // true if refs were updated on remote
}

// ConflictStrategy determines how shared-path conflicts are resolved during replay.
type ConflictStrategy string

const (
	StrategyLocalWins  ConflictStrategy = "local_wins"
	StrategyRemoteWins ConflictStrategy = "remote_wins"
	// StrategyRefuse resolves nothing: it collects every conflicting path and
	// aborts the merge with a *MergeConflictError before a ref is touched.
	// For a merge a human has to adjudicate — landing an experiment on the
	// agent branch — where picking either side silently would discard work
	// nobody chose to discard.
	//
	// The VALUE is non-empty on purpose. mergeIntoBranchLocked rewrites an
	// empty strategy to StrategyLocalWins, so a zero-value "refuse" would be
	// silently downgraded to the very resolution it exists to prevent.
	StrategyRefuse ConflictStrategy = "refuse"
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

// ReadFactOpts controls which version of a fact to read.
// nil opts reads from branch HEAD (the common case).
type ReadFactOpts struct {
	AtCommit     string // read at a specific commit hash (branch HEAD ignored)
	BeforeCommit string // read the last version before this commit (for retracts)
	WithHash     bool   // populate BlobHash in the result
}

// ReadFactResult holds the content and optional metadata from ReadFact.
type ReadFactResult struct {
	Content    string
	BlobHash   string // only populated when WithHash is set
	FromCommit string // only populated when BeforeCommit is used
}

// WriteFactResult holds the commit and blob hashes from a write operation.
type WriteFactResult struct {
	CommitHash string
	BlobHash   string
}
