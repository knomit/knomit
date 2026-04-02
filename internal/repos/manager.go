package repos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// Deps holds all shared resources needed to open and manage repos.
type Deps struct {
	Cfg         config.Config
	Signer      ssh.Signer
	AgentBranch string
	Embedder    store.BatchEmbedder // nil if unavailable
	KeyPath     string
	// OnRepoReady is called after a repo is opened or its store is swapped.
	// The web layer uses this to wire MCP handlers onto the repo.
	OnRepoReady func(ri *RepoInstance)
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

// Ontology returns the loaded ontology (may be nil before Boot).
func (m *Manager) Ontology() *fact.Ontology { return m.ontology }

// SetOnRepoReady sets the callback invoked after a repo is opened or swapped.
func (m *Manager) SetOnRepoReady(fn func(*RepoInstance)) { m.deps.OnRepoReady = fn }

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
		if ri.syncCancel != nil {
			ri.syncCancel()
		}
	}

	// Pass 2: wait for loops to finish, then shut down each repo's resources.
	for _, ri := range instances {
		if ri.syncWg != nil {
			ri.syncWg.Wait()
		}
		if ri.hub != nil {
			ri.hub.Shutdown()
		}
		if ri.closeFn != nil {
			ri.closeFn()
		}
	}
}

// Boot opens all repositories under cfg.Home/repos/.
// Phase 1: opens knomit.db, loads ontology, wires MCP.
// Phase 2: discovers and opens remaining *.db files.
func (m *Manager) Boot() error {
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	// Phase 1: open knomit.
	defaultDB := filepath.Join(reposDir, "knomit.db")
	knomitRI, err := m.openOne("knomit", defaultDB, true)
	if err != nil {
		return fmt.Errorf("open default repo: %w", err)
	}

	// Load ontology from knomit repo's git store.
	readResult, readErr := knomitRI.svc.ReadFact(context.Background(), m.deps.AgentBranch, "domains/ontology.yaml", nil)
	if readErr != nil {
		log.Warn().Msg("domains/ontology.yaml not found, using default ontology")
		m.ontology = fact.DefaultOntology()
	} else {
		m.ontology, err = fact.ParseOntology([]byte(readResult.Content))
		if err != nil {
			knomitRI.closeFn()
			return fmt.Errorf("parse ontology: %w", err)
		}
	}

	if m.deps.OnRepoReady != nil {
		m.deps.OnRepoReady(knomitRI)
	}
	m.Set("knomit", knomitRI)

	// Phase 2: discover remaining repos.
	dbFiles, _ := filepath.Glob(filepath.Join(reposDir, "*.db"))
	sort.Strings(dbFiles)
	for _, dbPath := range dbFiles {
		base := filepath.Base(dbPath)
		name := strings.TrimSuffix(base, ".db")
		if name == "knomit" {
			continue
		}
		if !isValidRepoName(name) {
			log.Warn().Str("file", base).Msg("skipping db with invalid repo name")
			continue
		}
		if err := m.Add(name, dbPath); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("skipping repo")
		}
	}
	return nil
}

// Add opens a single repository and registers it under name.
// Uses the ontology already loaded by Boot (or nil if Boot not yet called).
func (m *Manager) Add(name, dbPath string) error {
	ri, err := m.openOne(name, dbPath, false)
	if err != nil {
		return err
	}
	if m.deps.OnRepoReady != nil {
		m.deps.OnRepoReady(ri)
	}
	m.Set(name, ri)
	return nil
}

// ---------- private helpers ----------

// openOne initialises a single repo from a SQLite database file.
// If isDefault is true and no git data exists, the repo is initialised
// from scratch (or cloned from origin). Non-default repos that fail to
// open are returned as errors so the caller can skip them gracefully.
func (m *Manager) openOne(name, dbPath string, isDefault bool) (*RepoInstance, error) {
	b := repoBuilder{
		name:        name,
		dbPath:      dbPath,
		isDefault:   isDefault,
		cfg:         m.deps.Cfg,
		signer:      m.deps.Signer,
		agentBranch: m.deps.AgentBranch,
		embedder:    m.deps.Embedder,
		keyPath:     m.deps.KeyPath,
		ctx:         m.ctx,
	}

	if err := b.openStore(); err != nil {
		return nil, err
	}
	if err := b.openGit(); err != nil {
		b.close()
		return nil, err
	}
	b.ensureBranch()
	b.setupIndex()
	b.seedWatermarks()

	return b.build(), nil
}


// remoteAuthFromRecord builds a RemoteAuthConfig from a stored remote record,
// falling back to the global config for fields not set in the record.
func remoteAuthFromRecord(remote *store.Remote, fallback config.RemoteAuthConfig) config.RemoteAuthConfig {
	cfg := fallback
	if remote.AuthMethod != "" {
		cfg.AuthMethod = remote.AuthMethod
	}
	if remote.AuthToken != "" {
		if cfg.AuthMethod == "basic" {
			// token field stores user:password
			if parts := strings.SplitN(remote.AuthToken, ":", 2); len(parts) == 2 {
				cfg.User = parts[0]
				cfg.Password = parts[1]
			}
		} else {
			cfg.Token = remote.AuthToken
		}
	}
	return cfg
}

// isValidRepoName checks that a repo name contains only lowercase letters,
// digits, hyphens, or underscores.
func isValidRepoName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
