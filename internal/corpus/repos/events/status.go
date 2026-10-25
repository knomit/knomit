package events

// The index states a repo, and a create job mirroring it, can report.
// Empty means the job never reached indexing (it failed earlier, or is still
// before it).
const (
	IndexStateIndexing = "indexing"
	IndexStateReady    = "ready"
	IndexStateError    = "error"
)

// Status is the public projection of a repo's lifecycle status: what a
// consumer outside the lifecycle machine reads. The machine derives it from
// its cursor and the Index result and keeps its own bookkeeping beside it.
type Status struct {
	// Stage is populate|open|identify|index|serve|sync while a walk is in
	// flight, ready once it finished, else unavailable|closed.
	Stage string
	Index IndexStatus
	// IndexRunning reports whether the index job is in flight — what makes
	// Attach, Detach and Swap answer ErrIndexing.
	IndexRunning bool
	Sync         struct {
		Running bool
		Origin  string
	}
	Serve struct{ Running bool }
	// Err is the cause of Unavailable.
	Err error
}

// IndexStatus is the index half of Status. State is indexing|ready|error.
type IndexStatus struct {
	State  string
	Done   int
	Total  int
	Reason string
}
