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

type LogEntryWithTags struct {
	Commit    string     `json:"commit"`
	Date      string     `json:"date"`
	Message   string     `json:"message"`
	Operation string     `json:"operation,omitempty"`
	Files     FileCounts `json:"files,omitempty"`
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

// syncResult is returned by Sync to report what happened during synchronization.
type syncResult struct {
	Synced      bool   // true if tree changed (merge or fast-forward)
	FastForward bool   // true if fast-forward (no merge commit)
	MergeCommit string // hash of merge commit (empty if ff or no-op)
}

// pushResult is returned by Push to report what happened.
type pushResult struct {
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
