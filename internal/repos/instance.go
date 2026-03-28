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
	ListDir(branch, path string) ([]git.DirEntry, error)
	ReadFile(branch, path string) (string, error)
	ReadFileAtCommit(branch, path, commitHash string) (string, error)
	ReadFileLastCommit(branch, path, beforeCommitHash string) (content string, fromCommit string, err error)
	WriteFile(branch, path, content, message, operation string) (commitHash, blobHash string, err error)
	DeleteFile(branch, path, message, operation string) (commitHash string, err error)
	Log(branch, path string) ([]git.LogEntry, error)
	LogPaginated(branch, path string, limit int, after, from, before string) ([]git.LogEntryWithTags, string, string, error)
	CommitDetail(commitHash string) (*git.CommitDetailResult, error)
	Activity(branch, path string) (git.ActivityResult, error)
	HeadCommit(branch string) (string, error)
	ListAll(branch string) ([]string, error)
}

// SearchIndex is the narrow search/index interface needed by query handlers.
// Accepts *store.Index at runtime.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	GetByPath(path string) (*store.FactWithBody, error)
	GetLastCommit(branch string) (string, error)
	Stats(pathPrefix string) (store.StatsResult, error)
	Completions(category, prefix string, limit int) ([]string, error)
	ExplainFact(path string) (store.ExplainResult, error)
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
	AgentBranch string // the branch this repo writes to (e.g. machine/<hostname>)
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
