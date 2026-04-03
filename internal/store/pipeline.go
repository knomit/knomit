// Pipeline session and work item CRUD for LLM-driven pipelines (review, hypothesize, etc.).
package store

// PipelineSession represents an active pipeline session for a tool on a branch.
type PipelineSession struct {
	ID        string
	Tool      string
	Branch    string
	Status    string // "active", "completed", "abandoned"
	CreatedAt string
	UpdatedAt string
}

// PipelineWorkItem represents a single work item within a pipeline session.
type PipelineWorkItem struct {
	ID         int64
	SessionID  string
	StepType   string // "prune", "distill", "reflect", "hypothesize", etc.
	ClusterKey string
	FactsJSON  string
	Response   *string // nil until answered
	Priority   float64
	Depth      int // RAPTOR depth level (0 = initial)
	CreatedAt  string
}
