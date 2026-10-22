package web

import (
	"sync"
	"time"
)

// JobProgress tracks progress for an async job.
type JobProgress struct {
	Current int `json:"current"`
	Total   int `json:"total"`
}

// JobEntry is a record of a single async job in the registry.
type JobEntry struct {
	ID        string
	Kind      string // "synthesis-run" or "index-rebuild"
	State     string // "running", "done", "error"
	CreatedAt time.Time
	UpdatedAt time.Time
	Progress  *JobProgress
	Error     string
}

// JobRegistry tracks active and recently-completed async jobs in memory.
// It is keyed by job ID and is safe for concurrent use.
type JobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*JobEntry
}

// NewJobRegistry creates an empty registry.
func NewJobRegistry() *JobRegistry {
	return &JobRegistry{jobs: make(map[string]*JobEntry)}
}

// Register adds a new job in "running" state and returns it.
func (jr *JobRegistry) Register(id, kind string) *JobEntry {
	e := &JobEntry{
		ID:        id,
		Kind:      kind,
		State:     "running",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	jr.mu.Lock()
	jr.jobs[id] = e
	jr.mu.Unlock()
	return e
}

// Complete marks a job as done or error.
func (jr *JobRegistry) Complete(id, state, errMsg string) {
	jr.mu.Lock()
	if e, ok := jr.jobs[id]; ok {
		e.State = state
		e.Error = errMsg
		e.UpdatedAt = time.Now()
	}
	jr.mu.Unlock()
}

// SetProgress updates the progress counters for a running job.
func (jr *JobRegistry) SetProgress(id string, current, total int) {
	jr.mu.Lock()
	if e, ok := jr.jobs[id]; ok {
		e.Progress = &JobProgress{Current: current, Total: total}
		e.UpdatedAt = time.Now()
	}
	jr.mu.Unlock()
}

// Get returns the entry for id, or nil if not found.
func (jr *JobRegistry) Get(id string) *JobEntry {
	jr.mu.Lock()
	e := jr.jobs[id]
	jr.mu.Unlock()
	return e
}

// List returns all jobs of the given kind, sorted by creation time (newest first).
func (jr *JobRegistry) List(kind string) []*JobEntry {
	jr.mu.Lock()
	defer jr.mu.Unlock()
	var out []*JobEntry
	for _, e := range jr.jobs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	// Sort newest first.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Delete removes a job from the registry. It is a no-op if the id is unknown.
func (jr *JobRegistry) Delete(id string) {
	jr.mu.Lock()
	delete(jr.jobs, id)
	jr.mu.Unlock()
}
