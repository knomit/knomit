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
	// DisableBackgroundSync suppresses the background pull and push loops
	// that would otherwise run on every managed repo. Tests use this to
	// prevent non-deterministic sync/push behavior — the loops call
	// doSync/doPush immediately on startup which can race with test
	// assertions about remote state. Production leaves this unset.
	DisableBackgroundSync bool
}

// RescanError records a single per-repo failure during Manager.Rescan.
// Top-level errors (e.g. unreadable repos directory) are returned via the
// second return value of Rescan, not in this slice.
type RescanError struct {
	Repo string
	Err  error
}

// RescanResult is the outcome of a Manager.Rescan call.
//
//   - Added: repo names that were not previously registered and were
//     successfully opened during this call.
//   - Skipped: repo names already registered before this call.
//   - Errors: per-repo Add failures; other repos still attempted.
//
// On successful return, all three slices are non-nil; empty slices remain
// empty (callers/JSON encoders can rely on []string{} rather than nil).
// When Rescan returns a non-nil error, the caller should not inspect the
// result — the zero RescanResult{} is returned.
type RescanResult struct {
	Added   []string
	Skipped []string
	Errors  []RescanError
}

// Manager owns the full lifecycle of all registered repositories:
// discovery, initialisation, MCP wiring, sync loop management, the
// background cluster-cache warmer, and shutdown. Callers drive the
// lifecycle via Start/Close — internals (sync loops, cluster checker)
// are not exposed.
type Manager struct {
	mu    sync.RWMutex
	repos map[string]*RepoInstance
	ctx   context.Context
	deps  Deps

	// clusterCheckerStop is set by Start when the background cluster
	// cache warmer is launched, and invoked by Close to wind it down.
	// nil when Start hasn't been called or the checker is disabled
	// (cluster_cache.check_interval = 0).
	clusterCheckerStop func()

	// sessionReaperStop is set by Start when the background idle-session
	// reaper is launched, and invoked by Close to wind it down. nil only when
	// Start hasn't been called — the reaper itself is never disabled (see
	// parseSessionReaperConfig).
	sessionReaperStop func()

	// rescanMu serialises concurrent Rescan calls so the same .db cannot
	// be opened twice in a race. Independent of mu — Rescan reads m.repos
	// via Get/Set, which take mu themselves.
	rescanMu sync.Mutex

	// inflightMu guards creating and creatingOrigins — the sets of repo names
	// and origin URLs currently being brought into the active map by a Create or
	// Restore. They are the mutual-exclusion gate that keeps two concurrent
	// operations from racing on the same name (→ duplicate registration) or the
	// same origin (→ two active repos sharing one remote).
	inflightMu      sync.Mutex
	creating        map[string]struct{}
	creatingOrigins map[string]struct{}
}

// ResolveAuth resolves a transport.AuthMethod for the given config and remote
// URL, using the manager's own key path as the SSH key fallback. It is the
// clone boundary for the immediate-clone paths (repo create and origin-session
// test), so it also enforces the local-origin policy here: an origin that the
// LocalOriginRoot gate rejects fails before any clone is attempted. Deferred
// clones (sync of a stored remote) are gated at write time via
// ValidateLocalOrigin instead.
func (m *Manager) ResolveAuth(cfg config.RemoteAuthConfig, url string) (transport.AuthMethod, error) {
	if err := m.ValidateLocalOrigin(url); err != nil {
		return nil, err
	}
	return resolveAuthWithOrigin(cfg, m.deps.KeyPath, url)
}

// New returns an uninitialised Manager. Call Boot to open repos.
func New(ctx context.Context, deps Deps) *Manager {
	return &Manager{
		repos:           make(map[string]*RepoInstance),
		ctx:             ctx,
		deps:            deps,
		creating:        make(map[string]struct{}),
		creatingOrigins: make(map[string]struct{}),
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

// Close gracefully stops all registered repositories and any background
// goroutines Start launched (currently the cluster-cache warmer).
//
// Two-pass repo shutdown: cancel all sync loops first so they wind down
// concurrently, then wait and release resources repo by repo. The
// cluster checker is stopped before the sync passes because its
// Service-side reads rely on the repos still being open. Returns nil
// today; the error return matches io.Closer for forward compatibility.
func (m *Manager) Close() error {
	if m.clusterCheckerStop != nil {
		m.clusterCheckerStop()
		m.clusterCheckerStop = nil
	}
	if m.sessionReaperStop != nil {
		m.sessionReaperStop()
		m.sessionReaperStop = nil
	}

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
	return nil
}

// Start opens all repositories under cfg.Home/repos/ and launches the
// background cluster-cache warmer. trunk.db is opened first; remaining
// *.db files are discovered and opened. The warmer's behaviour comes
// from m.deps.Cfg.ClusterCache; check_interval=0 disables it. Callers
// must pair Start with a Close.
func (m *Manager) Start() error {
	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("create repos dir: %w", err)
	}

	// Open the default repo with isDefault=true so that initDefaultGit is
	// called on first run (no git data in a fresh DB).
	defaultDB := filepath.Join(reposDir, config.DefaultRepoName+".db")
	ri, err := m.openOne(config.DefaultRepoName, defaultDB, true)
	if err != nil {
		return fmt.Errorf("open default repo: %w", err)
	}
	m.Set(config.DefaultRepoName, ri)

	dbFiles, _ := filepath.Glob(filepath.Join(reposDir, "*.db"))
	sort.Strings(dbFiles)
	for _, dbPath := range dbFiles {
		base := filepath.Base(dbPath)
		if store.IsSessionDBFile(base) {
			continue // ephemeral session sidecar, not a repo
		}
		name := strings.TrimSuffix(base, ".db")
		if name == config.DefaultRepoName {
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

	// Launch the background cluster-cache warmer. Returning the error
	// here means a misconfigured cluster_cache block surfaces at boot
	// rather than silently disabling the warmer.
	checkerCfg, err := parseClusterCheckerConfig(m.deps.Cfg.ClusterCache)
	if err != nil {
		return fmt.Errorf("cluster checker config: %w", err)
	}
	m.clusterCheckerStop = m.startClusterChecker(checkerCfg)

	// Launch the background idle-session reaper. As with the cluster checker,
	// a misconfigured session block surfaces at boot rather than silently
	// disabling the reaper.
	reaperCfg, err := parseSessionReaperConfig(m.deps.Cfg.Session)
	if err != nil {
		return fmt.Errorf("session reaper config: %w", err)
	}
	m.sessionReaperStop = m.startSessionReaper(reaperCfg)
	return nil
}

// Add opens a single repository and registers it under name.
// Each repo loads its own ontology from its git store during initialization.
func (m *Manager) Add(name, dbPath string) error {
	ri, err := m.openOne(name, dbPath, false)
	if err != nil {
		return err
	}
	m.Set(name, ri)
	return nil
}

// Rescan re-discovers repos under <home>/repos/. Any *.db file whose name
// matches isValidRepoName and is not already registered is opened via Add.
// Already-registered repos are reported in Skipped and otherwise untouched.
//
// This is the runtime counterpart of the discovery loop inside Start: it
// lets a running server pick up new repos created by `knomit init` without
// a restart. Removed or replaced .db files are NOT handled — see the
// design doc for the rationale.
//
// Concurrent calls are serialised by rescanMu. On success the returned
// slices are always non-nil (possibly empty). The error return is non-nil
// only when the repos directory cannot be read; in that case the returned
// RescanResult is the zero value. Per-repo Add failures appear in
// result.Errors and do not abort the scan.
func (m *Manager) Rescan() (RescanResult, error) {
	m.rescanMu.Lock()
	defer m.rescanMu.Unlock()

	reposDir := filepath.Join(m.deps.Cfg.Home, "repos")
	if _, err := os.Stat(reposDir); err != nil {
		return RescanResult{}, fmt.Errorf("stat repos dir: %w", err)
	}

	dbFiles, err := filepath.Glob(filepath.Join(reposDir, "*.db"))
	if err != nil {
		// filepath.Glob can only return ErrBadPattern, which is unreachable
		// for our literal pattern — but keep the guard for forward safety.
		return RescanResult{}, fmt.Errorf("glob repos dir: %w", err)
	}
	sort.Strings(dbFiles)

	result := RescanResult{
		Added:   []string{},
		Skipped: []string{},
		Errors:  []RescanError{},
	}

	for _, dbPath := range dbFiles {
		base := filepath.Base(dbPath)
		if store.IsSessionDBFile(base) {
			continue // ephemeral session sidecar, not a repo
		}
		name := strings.TrimSuffix(base, ".db")
		if !isValidRepoName(name) {
			continue
		}
		if m.Get(name) != nil {
			result.Skipped = append(result.Skipped, name)
			continue
		}
		if err := m.Add(name, dbPath); err != nil {
			result.Errors = append(result.Errors, RescanError{Repo: name, Err: err})
			log.Warn().Err(err).Str("repo", name).Msg("rescan: add failed")
			continue
		}
		result.Added = append(result.Added, name)
		log.Info().Str("repo", name).Msg("rescan: opened")
	}
	return result, nil
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

	ri := b.build()

	// Synchronous open for test harnesses (DisableBackgroundSync): build the
	// index and activate inline so the index is ready when openOne returns —
	// preserving the open→index-ready contract many tests rely on.
	if b.disableBackgroundSync {
		ok := healIndexBranches(b.ctx, b.svc.IndexManager(), b.name, b.indexBranches, b.indexStale, nil)
		b.activate()
		if ok {
			ri.markIndexReady()
		} else {
			ri.markIndexFailed()
		}
		return ri, nil
	}

	// Production: the heavy initial index runs in the BACKGROUND. The store is
	// already live, so the HTTP server / UI come up immediately and reads work
	// progressively (partial until "ready"). The remote sync loops start only
	// after indexing (b.activate). The heal itself holds lockBranch per branch
	// (Rebuild self-locks; the incremental path uses SyncLocked), so a
	// concurrent inline write or the live commit observer (which also uses
	// SyncLocked) is serialized with it rather than racing the index watermark.
	// b.syncCtx is cancelled by shutdown (Archive) and by Manager.Close
	// (via b.ctx), so a close mid-index aborts the heal and skips activation.
	//
	// The heal goroutine is registered with b.syncWg so every teardown path
	// (Manager.Close, Archive→shutdown, SwapStore) — each of which does
	// syncWg.Wait() BEFORE svc.Close() — waits for the heal to finish before
	// the SQLite handle is closed. Without this the close would race in-flight
	// index SQL on the same *sql.DB ("database is closed"). The Add happens
	// here (synchronously, before openOne returns), so it is ordered before any
	// teardown Wait; b.activate's own syncWg.Add runs while this count is still
	// held, so the counter never transiently hits zero.
	ri.markIndexing()
	b.syncWg.Add(1)
	go func() {
		defer b.syncWg.Done()
		progress := func(_ string, done, total int) { ri.setIndexProgress(done, total) }
		ok := healIndexBranches(b.syncCtx, b.svc.IndexManager(), b.name, b.indexBranches, b.indexStale, progress)
		if b.syncCtx.Err() != nil {
			// Repo was closed/cancelled mid-index — a clean shutdown, not a
			// failure. Skip activation and leave the state as-is; the instance
			// is being torn down and its status is no longer observed.
			return
		}
		// Activate the sync loops even on a failed heal so the reconcile/push
		// loops can retry and recover; the index state still reflects that the
		// initial heal did not fully complete.
		b.activate()
		if ok {
			ri.markIndexReady()
		} else {
			ri.markIndexFailed()
		}
	}()

	return ri, nil
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
