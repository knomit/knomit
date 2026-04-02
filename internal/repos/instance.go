package repos

import (
	"context"
	"net/http"
	"sync"

	"knomit/internal/store"
)

// StoreDeps bundles the lock-protected fields for read access via WithRead.
// All fields may be nil if the repo is not yet fully initialised.
// GS is populated from Svc in production; tests may set GS to a mock GitStore.
type StoreDeps struct {
	GS  store.GitStore
	Svc *store.Service
	Idx store.SearchIndex
	MCP map[string]http.Handler
}

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	mu          sync.RWMutex
	name        string
	dbPath      string
	agentBranch string
	gsOverride  store.GitStore  // test-only: overrides svc as GS in StoreDeps
	svc         *store.Service
	idx         store.SearchIndex
	hub         *TaskHub
	syncCancel  context.CancelFunc
	syncWg      *sync.WaitGroup
	mcpHandlers map[string]http.Handler
	startSync   func(url string) error
	closeFn     func()
}

// WithRead calls fn with all lock-protected fields under a read lock.
// This is the only way external code may access gs, svc, idx, mcpHandlers,
// and synthDeps.
func (ri *RepoInstance) WithRead(fn func(StoreDeps)) {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	gs := store.GitStore(ri.svc)
	if ri.gsOverride != nil {
		gs = ri.gsOverride
	}
	fn(StoreDeps{
		GS:  gs,
		Svc: ri.svc,
		Idx: ri.idx,
		MCP: ri.mcpHandlers,
	})
}

// SetMCPHandlers replaces the MCP handler map under a write lock.
func (ri *RepoInstance) SetMCPHandlers(h map[string]http.Handler) {
	ri.mu.Lock()
	ri.mcpHandlers = h
	ri.mu.Unlock()
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
	GS          store.GitStore
	Svc         *store.Service
	Idx         store.SearchIndex
	Hub         *TaskHub
	MCP         map[string]http.Handler
	StartSync   func(url string) error
}

// NewTestInstanceWithDeps creates a RepoInstance pre-populated with the given
// dependencies. Intended for handler/integration tests in sibling packages.
// Production code must use Manager.openOne instead.
func NewTestInstanceWithDeps(cfg TestInstanceConfig) *RepoInstance {
	sc := cfg.StartSync
	return &RepoInstance{
		name:        cfg.Name,
		agentBranch: cfg.AgentBranch,
		gsOverride:  cfg.GS,
		svc:         cfg.Svc,
		idx:         cfg.Idx,
		hub:         cfg.Hub,
		mcpHandlers: cfg.MCP,
		startSync:   sc,
		syncCancel:  func() {},
		syncWg:      &sync.WaitGroup{},
	}
}
