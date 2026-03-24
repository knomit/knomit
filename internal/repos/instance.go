package repos

import (
	"context"
	"net/http"
	"sync"

	"knomit/internal/git"
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
// and the sync task handler. Accepts *git.Store at runtime.
type GitStore interface {
	ListDir(path string) ([]git.DirEntry, error)
	ReadFile(path string) (string, error)
	ReadFileAtCommit(path, commitHash string) (string, error)
	ReadFileLastCommit(path, beforeCommitHash string) (content string, fromCommit string, err error)
	WriteFile(path, content, message, operation string) (commitHash, blobHash string, err error)
	Log(path string) ([]git.LogEntry, error)
	LogPaginated(path string, limit int, after string) ([]git.LogEntryWithTags, string, error)
	CommitDetail(commitHash string) (*git.CommitDetailResult, error)
	Activity(path string) (git.ActivityResult, error)
	HeadCommit() (string, error)
	Branch() string
	ListAll() ([]string, error)
}

// SearchIndex is the narrow search/index interface needed by query handlers.
// Accepts *store.Index at runtime.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	GetByPath(path string) (*store.FactWithBody, error)
	GetLastCommit(branch string) (string, error)
	Stats(pathPrefix string) (store.StatsResult, error)
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

// RLock acquires a read lock protecting GS, Svc, and Idx.
// Call RUnlock when done.
func (ri *RepoInstance) RLock() { ri.mu.RLock() }

// RUnlock releases the read lock.
func (ri *RepoInstance) RUnlock() { ri.mu.RUnlock() }

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	mu          sync.RWMutex   // protects GS, Svc, Idx during SwapStore
	Name        string
	DBPath      string // path to the SQLite database file
	GS          GitStore
	Svc         *store.Service
	Idx         SearchIndex
	Hub         *TaskHub
	SyncCancel  context.CancelFunc
	SyncWg      *sync.WaitGroup
	MCPHandlers map[string]http.Handler // profile -> MCP handler
	SynthDeps   *SynthDeps             // nil if no LLM configured

	// StartSync is called by the origin handler to activate sync/push loops
	// after a remote is configured. Set during initialization.
	// Takes the remote URL and returns an error if activation fails.
	StartSync func(url string) error

	// Close stops the observer and closes the store. Set during initialization.
	Close func()
}
