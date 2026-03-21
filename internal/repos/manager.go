package repos

import (
	"context"
	"sort"
	"sync"

	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/llm"
)

// Deps holds all shared resources needed to open and manage repos.
type Deps struct {
	Cfg         config.Config
	Signer      ssh.Signer
	AgentBranch string
	Embedder    *embeddings.Embedder // nil if unavailable
	LLM         llm.LLMAdapter       // nil if unavailable
	KeyPath     string
}

// Manager owns the full lifecycle of all registered repositories:
// discovery, initialisation, MCP wiring, sync loop management, and shutdown.
type Manager struct {
	mu       sync.RWMutex
	repos    map[string]*RepoInstance
	ctx      context.Context
	deps     Deps
	ontology *fact.Ontology // loaded during Boot; used by setupMCP and Add
}

// New returns an uninitialised Manager. Call Boot to open repos.
func New(ctx context.Context, deps Deps) *Manager {
	return &Manager{
		repos: make(map[string]*RepoInstance),
		ctx:   ctx,
		deps:  deps,
	}
}

// Get returns the RepoInstance for name, or nil if not found.
func (m *Manager) Get(name string) *RepoInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.repos[name]
}

// Set registers a RepoInstance under the given name.
func (m *Manager) Set(name string, ri *RepoInstance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos[name] = ri
}

// Replace swaps the RepoInstance for name and returns the old instance
// (or nil if there was none) so the caller can clean it up.
func (m *Manager) Replace(name string, ri *RepoInstance) *RepoInstance {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.repos[name]
	m.repos[name] = ri
	return old
}

// ForEach calls fn for every registered repo while holding a read lock.
func (m *Manager) ForEach(fn func(name string, ri *RepoInstance)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, ri := range m.repos {
		fn(name, ri)
	}
}

// Names returns a sorted snapshot of registered repo names.
func (m *Manager) Names() []string {
	m.mu.RLock()
	names := make([]string, 0, len(m.repos))
	for name := range m.repos {
		names = append(names, name)
	}
	m.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Shutdown gracefully stops all registered repositories.
// It performs a two-pass shutdown: cancel all sync loops first so they wind
// down concurrently, then wait and release resources repo by repo.
func (m *Manager) Shutdown() {
	m.mu.RLock()
	instances := make([]*RepoInstance, 0, len(m.repos))
	for _, ri := range m.repos {
		instances = append(instances, ri)
	}
	m.mu.RUnlock()

	// Pass 1: cancel all sync loops so they can wind down concurrently.
	for _, ri := range instances {
		if ri.SyncCancel != nil {
			ri.SyncCancel()
		}
	}

	// Pass 2: wait for loops to finish, then shut down each repo's resources.
	for _, ri := range instances {
		if ri.SyncWg != nil {
			ri.SyncWg.Wait()
		}
		if ri.Hub != nil {
			ri.Hub.Shutdown()
		}
		if ri.Close != nil {
			ri.Close()
		}
	}
}
