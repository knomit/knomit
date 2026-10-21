package model

// RevisionMeta is one revision of a fact path in its commit history.
type RevisionMeta struct {
	Commit      string
	CommittedAt int64  // Unix seconds
	Message     string // first line of the commit message
	Action      string // "added" | "modified"
}

// RevisionDiff is a revision's delta from the content it was edited from.
// Each field is present only when it changed; a nil RevisionDiff means "no
// tracked change" (e.g. the fact's creation, which has no predecessor).
// Stored as JSON in path_changes.diff and returned by knomit_explain as-is.
type RevisionDiff struct {
	Confidence []float64 `json:"confidence,omitempty"` // [old, new]
	Body       string    `json:"body,omitempty"`       // smaller of unified diff vs "+N/-M"
}
