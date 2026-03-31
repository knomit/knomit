package repos

import (
	"context"
	"net/http"
	"sync"

	"knomit/internal/llm"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// Embedder is the interface required of the embedding model across all repos
// subsystems. Satisfied by *embeddings.Embedder at runtime; injectable in tests.
type Embedder interface {
	Embed(text string) ([]float32, error)
	EmbedBatch(texts []string) ([][]float32, error)
}

// GitStore is the narrow git interface needed by read-only query handlers
// and the sync task handler. Satisfied by *store.Service at runtime.
type GitStore interface {
	ListDir(ctx context.Context, branch, path string) ([]store.DirEntry, error)
	ReadFile(ctx context.Context, branch, path string) (string, error)
	ReadFileAtCommit(ctx context.Context, branch, path, commitHash string) (string, error)
	ReadFileLastCommit(ctx context.Context, branch, path, beforeCommitHash string) (content string, fromCommit string, err error)
	WriteFile(ctx context.Context, branch, path, content, message, operation string) (commitHash, blobHash string, err error)
	DeleteFile(ctx context.Context, branch, path, message, operation string) (commitHash string, err error)
	Log(ctx context.Context, branch, path string) ([]store.LogEntry, error)
	LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]store.LogEntryWithTags, string, string, error)
	CommitDetail(ctx context.Context, commitHash string) (*store.CommitDetailResult, error)
	Activity(ctx context.Context, branch, path string) (store.ActivityResult, error)
	HeadCommit(ctx context.Context, branch string) (string, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
}

// SearchIndex is the narrow search/index interface needed by query handlers.
// Accepts *store.Index at runtime.
type SearchIndex interface {
	Search(ctx context.Context, branch string, q store.SearchQuery) ([]store.SearchResult, error)
	GetByPath(ctx context.Context, branch, path string) (*store.FactWithBody, error)
	GetLastCommit(ctx context.Context, branch string) (string, error)
	Stats(ctx context.Context, branch, pathPrefix string) (store.StatsResult, error)
	Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error)
	ExplainFact(ctx context.Context, branch, path string) (store.ExplainResult, error)
}

// SynthDeps bundles the dependencies needed by the synthesize handler.
// May be nil if no LLM is configured — the synth handler returns 503
// in that case rather than panicking.
type SynthDeps struct {
	GS       synthesize.GitStore
	Idx      synthesize.SearchIndex
	Embedder Embedder
	Adapter  llm.LLMAdapter
	Reviewer *synthesize.Reviewer
}

// StoreDeps bundles the lock-protected fields for read access via WithRead.
// All five fields may be nil if the repo is not yet fully initialised.
type StoreDeps struct {
	Svc   *store.Service
	Idx   SearchIndex
	MCP   map[string]http.Handler
	Synth *SynthDeps
}

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	mu          sync.RWMutex
	name        string
	dbPath      string
	agentBranch string
	svc         *store.Service
	idx         SearchIndex
	hub         *TaskHub
	syncCancel  context.CancelFunc
	syncWg      *sync.WaitGroup
	mcpHandlers map[string]http.Handler
	synthDeps   *SynthDeps
	startSync   func(url string) error
	closeFn     func()
}

// WithRead calls fn with all lock-protected fields under a read lock.
// This is the only way external code may access gs, svc, idx, mcpHandlers,
// and synthDeps.
func (ri *RepoInstance) WithRead(fn func(StoreDeps)) {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	fn(StoreDeps{
		Svc:   ri.svc,
		Idx:   ri.idx,
		MCP:   ri.mcpHandlers,
		Synth: ri.synthDeps,
	})
}

// withWrite calls fn under a write lock. Only used within the repos package
// (SwapStore, SetupMCP, StartSync closure).
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
	Svc         *store.Service
	Idx         SearchIndex
	Hub         *TaskHub
	MCP         map[string]http.Handler
	Synth       *SynthDeps
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
		svc:         cfg.Svc,
		idx:         cfg.Idx,
		hub:         cfg.Hub,
		mcpHandlers: cfg.MCP,
		synthDeps:   cfg.Synth,
		startSync:   sc,
		syncCancel:  func() {},
		syncWg:      &sync.WaitGroup{},
	}
}
