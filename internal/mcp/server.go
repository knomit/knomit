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

// GitStore is the interface the MCP tools require from internal/git.
// Only methods actually used by the tool handlers are listed here so that
// tests can use lightweight mocks.
type GitStore interface {
	ReadFile(path string) (string, error)
	ReadFileWithHash(path string) (content, blobHash string, err error)
	WriteFile(path, content, message string) (commitHash, blobHash string, err error)
	BatchWrite(files map[string]string, message string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFile(path, message string) (commitHash string, err error)
	FileExists(path string) (bool, error)
	ListDir(path string) ([]DirEntry, error)
	ListAll() ([]string, error)
	Log(path string) ([]LogEntry, error)
	Grep(pattern string) ([]string, error)
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit() (string, error)
	Tag(name string) error
	TagsContaining(hash string) ([]string, error)
	Branch() string
}

// SearchIndex is the interface the MCP tools require from internal/store.
type SearchIndex interface {
	Search(q SearchQuery) ([]SearchResult, error)
	GetByPath(path string) (*FactWithBody, error)
}

// NewServer creates a new MCP server with all knomit tools registered.
func NewServer(gs GitStore, idx SearchIndex, reviewer Reviewer, profile, ontologyRoot string, ontology *fact.Ontology) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile, ontologyRoot, ontology)),
	)

	s.AddTool(learnTool(), LearnHandler(gs, idx, ontologyRoot, ontology))
	s.AddTool(queryTool(), QueryHandler(gs, idx))
	s.AddTool(whyTool(), WhyHandler(gs, ontologyRoot))
	s.AddTool(updateTool(), UpdateHandler(gs, ontologyRoot))
	s.AddTool(exploreTool(ontologyRoot), ExploreHandler(gs, ontologyRoot))
	s.AddTool(retractTool(), RetractHandler(gs, ontologyRoot))

	if reviewer != nil {
		s.AddTool(reviewTool(), ReviewHandler(reviewer))
	}

	return s
}
