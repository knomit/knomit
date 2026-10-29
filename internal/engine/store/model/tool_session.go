package model

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
	SortKey    int
	State      string
}
