package model

import "errors"

// ErrSessionBusy wraps a DequeuePaths failure caused by the session DB staying
// locked past its busy timeout (SQLITE_BUSY / SQLITE_LOCKED). The dequeue was
// rolled back, so nothing was consumed and the same call may be retried.
// DequeuePaths and the FactSet writes (sessions.ToolIndex.CreateFactSet,
// TouchFactSet, DeleteFactSet, CloseFactSet: one BEGIN IMMEDIATE transaction each, rolled
// back on failure) return it: a busy error on a later write of a page (a
// re-enqueue after the dequeue committed) is NOT safe to retry and is never
// marked with it.
var ErrSessionBusy = errors.New("session store busy")

// ToolSession represents a paginated tool session for a branch.
type ToolSession struct {
	ID         string
	Tool       string
	Branch     string
	PathPrefix string
	Binding    string
	ReadSet    string
	// Owner is the endpoint + principal that minted the session; only the
	// same owner may resume it.
	Owner      string
	LastCommit string
	Status     string // "active", "completed", "abandoned"
	CreatedAt  string
	UpdatedAt  string
}

// QueueItem represents a single item in a tool session's work queue. SortKey is
// the SQL-orderable consume order (breadth-first depth for explain/explore;
// rank index for query). State is an optional per-item JSON payload (query
// stores the frozen rank score here; the fact body is re-read from path+commit
// on resume, so the snapshot carries no body text).
type QueueItem struct {
	Path       string
	CommitHash string
	// Blob is the version's blob hash when the producer knows it (knomit_query
	// writes the hit's blob), "" otherwise. With CommitHash it names exactly the
	// version the cursor froze.
	Blob    string
	SortKey int
	State   string
}
