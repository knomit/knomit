// internal/okf/types.go
package okf

import (
	"time"

	"knomit/internal/fact"
)

// RepoIdentity is the machine-independent identity of the source repo.
// ID is the root commit hash in 12-hex wire form (never the repo name).
type RepoIdentity struct {
	ID string
}

// FactInput pairs a parsed fact with its authoring time, resolved by the
// caller from git history. Timestamp is the time of the commit that first
// introduced the fact's path.
type FactInput struct {
	Fact      fact.Fact
	Timestamp time.Time
}

// LogEntry is one changelog row for log.md. Kind is "Creation" or "Update".
type LogEntry struct {
	Date  time.Time // the commit time of the event
	Kind  string    // "Creation" | "Update"
	Title string    // the fact's title at the time (best-effort; current title is acceptable)
	Path  string    // the fact's knomit path, e.g. "kb/decisions/okf/x/ab12cd34.md"
}

// File is one rendered file in the bundle. Path is bundle-relative and
// forward-slashed (e.g. "decisions/okf/index.md").
type File struct {
	Path    string
	Content []byte
}

// Bundle is the complete in-memory OKF bundle. Files are unordered; callers
// that need determinism sort by Path.
type Bundle struct {
	Files []File
}
