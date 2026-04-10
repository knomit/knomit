package repos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
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
	// DisableBackgroundSync suppresses the background pull and push loops
	// that would otherwise run on every managed repo. Tests use this to
	// prevent non-deterministic sync/push behavior — the loops call
	// doSync/doPush immediately on startup which can race with test
	// assertions about remote state. Production leaves this unset.
	DisableBackgroundSync bool
}

// Manager owns the full lifecycle of all registered repositories:
// discovery, initialisation, MCP wiring, sync loop management, and shutdown.
type Manager struct {
	mu       sync.RWMutex
	repos    map[string]*RepoInstance
	ctx      context.Context
	deps     Deps
}

// ResolveAuth resolves a transport.AuthMethod for the given config and remote
// URL, using the manager's own key path as the SSH key fallback.
func (m *Manager) ResolveAuth(cfg config.RemoteAuthConfig, url string) (transport.AuthMethod, error) {
	return resolveAuthWithOrigin(cfg, m.deps.KeyPath, url)
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
		ri.mu.RLock()
		cancel := ri.syncCancel
		ri.mu.RUnlock()
		if cancel != nil {
			cancel()
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
// knomit.db is opened first; remaining *.db files are discovered and opened.
func (m *Manager) Boot() error {
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	// Open the default repo with isDefault=true so that initDefaultGit is
	// called on first run (no git data in a fresh DB).
	defaultDB := filepath.Join(reposDir, "knomit.db")
	ri, err := m.openOne("knomit", defaultDB, true)
	if err != nil {
		return fmt.Errorf("open default repo: %w", err)
	}
	if m.deps.OnRepoReady != nil {
		m.deps.OnRepoReady(ri)
	}
	m.Set("knomit", ri)

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
// Each repo loads its own ontology from its git store during initialization.
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
		name:                  name,
		dbPath:                dbPath,
		isDefault:             isDefault,
		cfg:                   m.deps.Cfg,
		signer:                m.deps.Signer,
		agentBranch:           m.deps.AgentBranch,
		embedder:              m.deps.Embedder,
		keyPath:               m.deps.KeyPath,
		ctx:                   m.ctx,
		disableBackgroundSync: m.deps.DisableBackgroundSync,
	}

	if err := b.openStore(); err != nil {
		return nil, err
	}
	if err := b.openGit(); err != nil {
		b.close()
		return nil, err
	}
	b.loadOntology()
	b.ensureBranch()
	b.setupIndex()
	b.seedWatermarks()

	return b.build(), nil
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
