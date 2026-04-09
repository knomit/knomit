package repos

import (
	"context"
	"sync"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	mu          sync.RWMutex
	name        string
	dbPath      string
	agentBranch string
	ontology    *fact.Ontology
	onCommit    func(string, string) // re-applied to new svc after SwapStore
	svc         *store.Service
	hub         *TaskHub
	syncCancel  context.CancelFunc
	syncWg      *sync.WaitGroup
	startSync   func(url string) error
	closeFn     func()
}

// WithRead calls fn with the store service under a read lock.
// This is the only way external code may access svc.
func (ri *RepoInstance) WithRead(fn func(*store.Service)) {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	fn(ri.svc)
}

// withWrite calls fn under a write lock. Only used within the repos package
// (SwapStore, StartSync closure).
func (ri *RepoInstance) withWrite(fn func()) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	fn()
}

// Name returns the repository name.
func (ri *RepoInstance) Name() string { return ri.name }

// AgentBranch returns the agent branch this repo writes to.
func (ri *RepoInstance) AgentBranch() string { return ri.agentBranch }

// Ontology returns the ontology loaded from this repo's git store at open time.
func (ri *RepoInstance) Ontology() *fact.Ontology { return ri.ontology }

// TaskHub returns the hub for broadcasting task status events.
func (ri *RepoInstance) TaskHub() *TaskHub { return ri.hub }

// ActivateSync starts sync and push loops for the given remote URL.
// Returns an error if the remote cannot be configured.
func (ri *RepoInstance) ActivateSync(url string) error {
	if ri.startSync == nil {
		return nil
	}
	return ri.startSync(url)
}

// Close stops the observer and closes the store.
func (ri *RepoInstance) Close() {
	if ri.closeFn != nil {
		ri.closeFn()
	}
}

// Verify runs the integrity check against the current store under the read
// lock so that a concurrent SwapStore cannot move the rug. Delegates to
// store.Service.Verify and stamps the report with this repo's name.
func (ri *RepoInstance) Verify(ctx context.Context, opts store.VerifyOpts) (store.IntegrityReport, error) {
	ri.mu.RLock()
	svc := ri.svc
	ri.mu.RUnlock()
	report, err := svc.Verify(ctx, opts)
	report.Repo = ri.name
	return report, err
}

// NewTestInstance creates a minimal RepoInstance for use in tests that
// exercise Manager operations (Set, Get, Replace, ForEach, Names, context).
// Production code must use Manager.openOne instead.
func NewTestInstance(name string) *RepoInstance {
	return &RepoInstance{
		name:       name,
		syncCancel: func() {},
		syncWg:     &sync.WaitGroup{},
	}
}

// TestInstanceConfig holds optional fields for NewTestInstanceWithDeps.
// Zero values are safe — nil fields are treated as "not configured".
type TestInstanceConfig struct {
	Name        string
	AgentBranch string
	Svc         *store.Service
	Ontology    *fact.Ontology
	Hub         *TaskHub
	StartSync   func(url string) error
}

// NewTestInstanceWithDeps creates a RepoInstance pre-populated with the given
// dependencies. Intended for handler/integration tests in sibling packages.
// Production code must use Manager.openOne instead.
func NewTestInstanceWithDeps(cfg TestInstanceConfig) *RepoInstance {
	return &RepoInstance{
		name:        cfg.Name,
		agentBranch: cfg.AgentBranch,
		svc:         cfg.Svc,
		ontology:    cfg.Ontology,
		hub:         cfg.Hub,
		startSync:   cfg.StartSync,
		syncCancel:  func() {},
		syncWg:      &sync.WaitGroup{},
	}
}
