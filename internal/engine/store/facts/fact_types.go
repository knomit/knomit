package facts

import (
	"errors"
)

type DirEntry struct {
	Name  string
	IsDir bool // true = subdirectory, false = .md file
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
//
// Unchanged reports a NO-OP write (#432): the content was byte-identical to
// the blob already at the path on the branch tip, so nothing was written — no
// object, no commit, no ref move, no commit notification. CommitHash is then
// the branch tip the write was compared against (NOT a new commit) and
// BlobHash the blob already there. A caller must never treat CommitHash as
// "the commit this write made" without checking Unchanged first.
type WriteFactResult struct {
	CommitHash string
	BlobHash   string
	Unchanged  bool
}

// ErrRepoReadOnly is returned by every authored-write door when the store has
// been marked read-only (a subscription). Sync merges are not authored writes
// and are not gated.
var ErrRepoReadOnly = errors.New("repo is read-only")

// ErrFactChanged is returned by a conditional write (WriteFactIfUnchanged)
// whose file no longer holds the blob the caller read: another write landed
// in between, and committing would silently discard it.
var ErrFactChanged = errors.New("fact changed since it was read")

// ErrInvalidPath is wrapped by every write door when it refuses the path
// itself: empty, containing "..", or one go-git would refuse to read back
// (#384). Nothing was written. Callers map it (IsInvalidPath) to a 400 rather
// than a 500: the path is the caller's mistake, not a store failure.
var ErrInvalidPath = errors.New("invalid path")
