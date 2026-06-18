package store

import "time"

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

type FileRecency struct {
	Path      string
	Timestamp time.Time
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
type AgentReconcileResult struct {
	Mode        Mode   `json:"mode"`
	NumReplayed int    `json:"num_replayed,omitempty"`
	NewTip      string `json:"new_tip,omitempty"`
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
)

// ReplayConfig controls replay behavior.
type ReplayConfig struct {
	Strategy          ConflictStrategy
	AgentBranch       string
	DefaultBranch     string
	UseExistingBranch bool // if true and AgentBranch exists on target, replay on top of it
	OnProgress        func(current, total int)
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
