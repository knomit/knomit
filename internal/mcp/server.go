// Package mcp implements the knomit MCP server.
package mcp

import (
	"github.com/mark3labs/mcp-go/server"

	"knomit/internal/fact"
	gitpkg "knomit/internal/git"
	storepkg "knomit/internal/store"
)

// Re-export supporting types so MCP tool handlers can use them without
// importing internal sub-packages directly.

// DirEntry is re-exported from internal/git.
type DirEntry = gitpkg.DirEntry

// LogEntry is re-exported from internal/git.
type LogEntry = gitpkg.LogEntry

// SearchQuery is re-exported from internal/store.
type SearchQuery = storepkg.SearchQuery

// SearchResult is re-exported from internal/store.
type SearchResult = storepkg.SearchResult

// FactRecord is re-exported from internal/store.
type FactRecord = storepkg.FactRecord

// FactWithBody is re-exported from internal/store.
type FactWithBody = storepkg.FactWithBody

// FileRecency is re-exported from internal/git.
type FileRecency = gitpkg.FileRecency

// ToolSession is re-exported from internal/store.
type ToolSession = storepkg.ToolSession

// QueueItem is re-exported from internal/store.
type QueueItem = storepkg.QueueItem

// GitStore is the interface the MCP tools require from internal/git.
// Only methods actually used by the tool handlers are listed here so that
// tests can use lightweight mocks.
type GitStore interface {
	ReadFile(path string) (string, error)
	ReadFileAtCommit(path, commitHash string) (string, error)
	ReadFileWithHash(path string) (content, blobHash string, err error)
	WriteFile(path, content, message, operation string) (commitHash, blobHash string, err error)
	BatchWrite(files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFile(path, message, operation string) (commitHash string, err error)
	FileExists(path string) (bool, error)
	ListDir(path string) ([]DirEntry, error)
	ListAll() ([]string, error)
	Log(path string) ([]LogEntry, error)
	Grep(pattern string) ([]string, error)
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit() (string, error)
	WalkChangedFiles(fromCommit, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error)
	Branch() string
}

// SearchIndex is the interface the MCP tools require from internal/store.
type SearchIndex interface {
	Search(q SearchQuery) ([]SearchResult, error)
	GetByPath(path string) (*FactWithBody, error)
}

// ToolSessionIndex is the interface tools require for session persistence.
type ToolSessionIndex interface {
	CreateToolSession(tool, branch, pathPrefix string) (*ToolSession, error)
	GetToolSession(id string) (*ToolSession, error)
	UpdateToolSession(id, lastCommit, status string) error
	GetSeenPaths(sessionID string) (map[string]bool, error)
	AddSeenPaths(sessionID string, paths []string) error
	GCToolSessions(tool, branch string, keep int) error
	EnqueuePaths(sessionID string, items []QueueItem) error
	DequeuePaths(sessionID string, limit int) ([]QueueItem, error)
	QueueSize(sessionID string) (int, error)
}

// NewServer creates a new MCP server with all knomit tools registered.
// If embedder is non-nil, the learn tool uses it for batch dedup embedding.
func NewServer(gs GitStore, idx SearchIndex, sessionIdx ToolSessionIndex, reviewer Reviewer, profile, ontologyRoot string, ontology *fact.Ontology, embedders ...BatchEmbedder) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile, ontologyRoot, ontology)),
	)

	s.AddTool(learnTool(), LearnHandler(gs, idx, ontologyRoot, ontology, embedders...))
	s.AddTool(queryTool(), QueryHandler(gs, idx))
	s.AddTool(explainTool(), ExplainHandler(gs, sessionIdx, ontologyRoot))
	s.AddTool(updateTool(), UpdateHandler(gs, ontologyRoot))
	s.AddTool(exploreTool(ontologyRoot), ExploreHandler(gs, sessionIdx, ontologyRoot))
	s.AddTool(retractTool(), RetractHandler(gs, ontologyRoot))

	if reviewer != nil {
		s.AddTool(reviewTool(), ReviewHandler(reviewer))
	}

	return s
}
