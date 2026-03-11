// Package mcp implements the knomit MCP server.
package mcp

import (
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	gitpkg "knomit/internal/git"
	"knomit/internal/llm"
	storepkg "knomit/internal/store"
)

// Re-export supporting types so MCP tool handlers can use them without
// importing internal sub-packages directly.

// DirEntry is re-exported from internal/git.
type DirEntry = gitpkg.DirEntry

// LogEntry is re-exported from internal/git.
type LogEntry = gitpkg.LogEntry

// SyncResult is re-exported from internal/git.
type SyncResult = gitpkg.SyncResult

// SearchQuery is re-exported from internal/store.
type SearchQuery = storepkg.SearchQuery

// SearchResult is re-exported from internal/store.
type SearchResult = storepkg.SearchResult

// FactRecord is re-exported from internal/store.
type FactRecord = storepkg.FactRecord

// GitReader is re-exported from internal/store (used by SearchIndex.Sync).
type GitReader = storepkg.GitReader

// GitStore is the interface the MCP tools require from internal/git.
// Only methods actually used by the tool handlers are listed here so that
// tests can use lightweight mocks.
type GitStore interface {
	ReadFile(path string) (string, error)
	WriteFile(path, content, message string) error
	BatchWrite(files map[string]string, message string) error
	DeleteFile(path, message string) error
	FileExists(path string) (bool, error)
	ListDir(path string) ([]DirEntry, error)
	ListAll() ([]string, error)
	Log(path string) ([]LogEntry, error)
	Grep(pattern string) ([]string, error)
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit() (string, error)
	Tag(name string) error
	Sync(remoteAuth interface{}) (SyncResult, error)
	TagsContaining(hash string) ([]string, error)
	Branch() string
}

// SearchIndex is the interface the MCP tools require from internal/store.
type SearchIndex interface {
	Search(q SearchQuery) ([]SearchResult, error)
	Upsert(r FactRecord) error
	Delete(path string) error
	Sync(g GitReader) error
	GetLastCommit() (string, error)
	SetLastCommit(hash string) error
}

// NewServer creates a new MCP server with all knomit tools registered.
// Tool handler implementations will be added in Tasks 16–18.
func NewServer(gs GitStore, idx SearchIndex, llmAdapter llm.LLMAdapter, profile string) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0")

	// Tool registrations will be added in subsequent tasks.
	// Placeholder to avoid "s declared and not used" errors:
	_ = mcpgo.NewTool // reference the mcp package to confirm the import is valid
	_ = gs
	_ = idx
	_ = llmAdapter
	_ = profile

	return s
}
