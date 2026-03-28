package repos

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/git"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/observe"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// Deps holds all shared resources needed to open and manage repos.
type Deps struct {
	Cfg         config.Config
	Signer      ssh.Signer
	AgentBranch string
	Embedder    Embedder // nil if unavailable; must implement store.Embedder and mcp.BatchEmbedder
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
	ontologyYAML, readErr := knomitRI.gs.ReadFile(m.deps.AgentBranch, "domains/ontology.yaml")
	if readErr != nil {
		log.Warn().Msg("domains/ontology.yaml not found, using default ontology")
		m.ontology = fact.DefaultOntology()
	} else {
		m.ontology, err = fact.ParseOntology([]byte(ontologyYAML))
		if err != nil {
			knomitRI.closeFn()
			return fmt.Errorf("parse ontology: %w", err)
		}
	}

	m.SetupMCP(knomitRI)
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
	m.SetupMCP(ri)
	m.Set(name, ri)
	return nil
}

// ---------- private helpers ----------

// openOne initialises a single repo from a SQLite database file.
// If isDefault is true and no git data exists, the repo is initialised
// from scratch (or cloned from origin). Non-default repos that fail to
// open are returned as errors so the caller can skip them gracefully.
func (m *Manager) openOne(name, dbPath string, isDefault bool) (*RepoInstance, error) {
	cfg := m.deps.Cfg
	signer := m.deps.Signer
	agentBranch := m.deps.AgentBranch
	embedder := m.deps.Embedder
	keyPath := m.deps.KeyPath
	ctx := m.ctx

	svc, err := store.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Set up credential encryption using the SSH private key.
	if keyData, readErr := os.ReadFile(keyPath); readErr == nil {
		if crypt, cryptErr := store.NewCrypt(keyData); cryptErr == nil {
			svc.SetCrypt(crypt)
		}
	}

	gs, err := git.OpenWithStorer(svc.GitStorer())
	if err != nil {
		if !isDefault {
			svc.Close()
			return nil, fmt.Errorf("open git: %w", err)
		}
		// Default repo — first run, init from remote or local.
		if cfg.Git.Origin != "" {
			auth, authErr := git.ResolveAuth(cfg.Remote, keyPath)
			if authErr != nil {
				svc.Close()
				return nil, fmt.Errorf("resolve auth: %w", authErr)
			}
			gs, err = git.InitFromRemote(svc.GitStorer(), cfg.Git.Origin, auth, agentBranch)
			if err != nil {
				svc.Close()
				return nil, fmt.Errorf("init from remote: %w", err)
			}
		} else {
			ont := fact.DefaultOntology()
			ontologyYAML, serErr := ont.Serialize()
			if serErr != nil {
				svc.Close()
				return nil, fmt.Errorf("serialize ontology: %w", serErr)
			}
			initFiles := map[string]string{
				"domains/ontology.yaml": string(ontologyYAML),
			}
			gs, err = git.InitWithStorer(svc.GitStorer(), initFiles, agentBranch)
			if err != nil {
				svc.Close()
				return nil, fmt.Errorf("init git: %w", err)
			}
		}
	}

	gs.SetSigner(signer)

	// Ensure the expected agent branch exists.
	if agentBranch != "" {
		if err := gs.CreateBranch(agentBranch, agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("branch create/ensure failed")
		}
	}

	// Seed remotes table for default repo on first startup.
	if isDefault && cfg.Git.Origin != "" {
		if err := svc.SetRemote("origin", cfg.Git.Origin, "main", 300, 300); err != nil {
			log.Warn().Err(err).Msg("failed to seed origin in remotes table")
		}
	}

	idx := svc.Index()
	if embedder != nil {
		idx.SetEmbedder(embedder)
	}

	// Initial index sync.
	if err := idx.Sync(gs, agentBranch); err != nil {
		log.Warn().Err(err).Str("repo", name).Msg("initial index sync failed")
	}

	// If there is no pipeline watermark for this branch, set it to HEAD so the
	// first run only processes facts written after this point. This covers
	// fresh init, branch switches (new machine / key rotation), and any other
	// scenario where an existing repo has no watermark for the current branch.
	for _, tool := range []string{"review", "hypothesize"} {
		if wm, _ := idx.GetPipelineWatermark(tool, agentBranch); wm == "" {
			if head, err := gs.HeadCommit(agentBranch); err == nil {
				if err := idx.SetPipelineWatermark(tool, agentBranch, head); err != nil {
					log.Warn().Err(err).Str("tool", tool).Msg("pipeline watermark: initial set failed")
				}
			}
		}
	}

	hub := NewTaskHub(ctx)

	// Observer: sync index + push SSE on every git commit.
	// The closure reads ri.GS and ri.Svc at call time so that after SwapStore
	// it operates on the current (open) database, not the original (closed) one.
	// ri is assigned below before any commits can fire.
	var ri *RepoInstance
	obs := observe.New(time.Second, func(hash string) {
		ri.mu.RLock()
		currentGS, ok := ri.gs.(*git.Store)
		currentSvc := ri.svc
		ri.mu.RUnlock()
		if !ok {
			return
		}
		if err := currentSvc.Index().Sync(currentGS, agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("observer sync failed")
		}
		hub.BroadcastStatus(hash)
	})
	gs.SetOnCommit(func(_, hash string) { obs.Notify(hash) })

	// Background remote sync + push goroutines.
	syncCtx, syncCancel := context.WithCancel(ctx)
	var syncWg sync.WaitGroup
	remote, _ := svc.GetRemote("origin")
	if remote != nil {
		authCfg := remoteAuthFromRecord(remote, cfg.Remote)
		auth, authErr := git.ResolveAuthWithOrigin(authCfg, keyPath, remote.URL)
		if authErr != nil {
			log.Warn().Err(authErr).Str("repo", name).Msg("remote: auth resolution failed")
		} else {
			gs.SetAuth(auth)
		}

		if err := gs.ConfigureRemote(remote.URL, remote.Branch); err != nil {
			log.Warn().Err(err).Str("repo", name).Msg("remote: configure failed")
		} else {
			syncWg.Add(2)
			go runSyncLoop(syncCtx, &syncWg, gs, svc, hub, remote, name, agentBranch)
			go runPushLoop(syncCtx, &syncWg, gs, svc, hub, remote, name, agentBranch)
		}
	}

	ri = &RepoInstance{
		name:        name,
		dbPath:      dbPath,
		agentBranch: agentBranch,
		gs:          gs,
		svc:         svc,
		idx:         idx,
		hub:         hub,
		syncCancel:  syncCancel,
		syncWg:      &syncWg,
	}
	ri.startSync = func(remoteURL string) error {
		// Use ri.gs and ri.svc (not captured gs/svc) so that after SwapStore
		// the sync loops operate on the current store, not the original one.
		ri.mu.RLock()
		currentGS, ok := ri.gs.(*git.Store)
		currentSvc := ri.svc
		ri.mu.RUnlock()
		if !ok {
			return fmt.Errorf("current store is not a *git.Store")
		}

		remote, err := currentSvc.GetRemote("origin")
		if err != nil || remote == nil {
			return fmt.Errorf("read remote: %w", err)
		}

		authCfg := remoteAuthFromRecord(remote, cfg.Remote)
		auth, authErr := git.ResolveAuthWithOrigin(authCfg, keyPath, remoteURL)
		if authErr != nil {
			return fmt.Errorf("resolve auth: %w", authErr)
		}
		currentGS.SetAuth(auth)

		if err := currentGS.ConfigureRemote(remoteURL, remote.Branch); err != nil {
			return fmt.Errorf("configure remote: %w", err)
		}

		// Stop existing sync/push loops (if any) before starting new ones.
		syncCancel()
		syncWg.Wait()

		// Create fresh context and update ri so shutdown cancels the right one.
		syncCtx, syncCancel = context.WithCancel(ctx)
		ri.syncCancel = syncCancel

		// Re-register the observer on the current git store so local writes
		// trigger index sync after a SwapStore replaced ri.gs.
		currentGS.SetOnCommit(func(_, hash string) { obs.Notify(hash) })

		syncWg.Add(2)
		go runSyncLoop(syncCtx, &syncWg, currentGS, currentSvc, hub, remote, name, agentBranch)
		go runPushLoop(syncCtx, &syncWg, currentGS, currentSvc, hub, remote, name, agentBranch)
		return nil
	}

	ri.closeFn = func() {
		obs.Stop()
		ri.mu.RLock()
		svc := ri.svc
		ri.mu.RUnlock()
		svc.Close()
	}

	return ri, nil
}

// SetupMCP wires MCP handlers onto ri using the manager's ontology and deps.
// It reads the current ri.gs and ri.svc to get concrete types, so it is safe
// to call after SwapStore to rebind MCP handlers to the new database.
// No-op if m.ontology is nil.
func (m *Manager) SetupMCP(ri *RepoInstance) {
	if m.ontology == nil {
		return
	}

	ri.mu.RLock()
	gs, ok := ri.gs.(*git.Store)
	svc := ri.svc
	ri.mu.RUnlock()
	if !ok {
		log.Warn().Msg("SetupMCP: ri.gs is not *git.Store, skipping")
		return
	}
	idx := svc.Index()

	ontologyRoot := m.deps.Cfg.OntologyRoot
	embedder := m.deps.Embedder
	llmAdapter := m.deps.LLM

	agentBranch := m.deps.AgentBranch
	reviewer := synthesize.NewReviewer(gs, idx, idx, embedder, nil, agentBranch)
	profiles := []string{"code", "chat", "generic"}
	mcpHandlers := make(map[string]http.Handler, len(profiles))
	for _, p := range profiles {
		var mcpSrv *mcpserver.MCPServer
		if embedder != nil {
			mcpSrv = mcp.NewServer(gs, idx, idx, idx, reviewer, p, ontologyRoot, m.ontology, agentBranch, embedder)
		} else {
			mcpSrv = mcp.NewServer(gs, idx, idx, idx, reviewer, p, ontologyRoot, m.ontology, agentBranch)
		}
		mcpHandlers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
	}

	var synthDeps *SynthDeps
	if llmAdapter != nil {
		synthReviewer := synthesize.NewReviewer(gs, idx, idx, embedder, nil, agentBranch)
		synthDeps = &SynthDeps{
			GS:       gs,
			Idx:      idx,
			Embedder: embedder,
			Adapter:  llmAdapter,
			Reviewer: synthReviewer,
		}
	}

	ri.withWrite(func() {
		ri.mcpHandlers = mcpHandlers
		ri.synthDeps = synthDeps
	})
}

// remoteAuthFromRecord builds a RemoteAuthConfig from a stored remote record,
// falling back to the global config for fields not set in the record.
func remoteAuthFromRecord(remote *store.Remote, fallback git.RemoteAuthConfig) git.RemoteAuthConfig {
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
