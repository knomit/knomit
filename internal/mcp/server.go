// Package mcp implements the knomit MCP server.
package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/server"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// GitStore is the interface the MCP tools require from internal/store.
// Only methods actually used by the tool handlers are listed here so that
// tests can use lightweight mocks.
type GitStore interface {
	ReadFact(ctx context.Context, branch, path string, opts *store.ReadFactOpts) (store.ReadFactResult, error)
	WriteFact(ctx context.Context, branch, path, content, message, operation string) (store.WriteFactResult, error)
	BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFact(ctx context.Context, branch, path, message string) (string, error)
	FactExists(ctx context.Context, branch, path string) (bool, error)
	ListDir(ctx context.Context, branch, path string) ([]store.DirEntry, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
	Log(ctx context.Context, branch, path string) ([]store.LogEntry, error)
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit(ctx context.Context, branch string) (string, error)
	WalkChangedFiles(ctx context.Context, branch, fromCommit, prefix string, seen map[string]bool, limit int) ([]store.FileRecency, string, error)
	LastCommitForPath(ctx context.Context, branch, path string) (string, error)
}

// SearchIndex is the interface the MCP tools require from internal/store.
type SearchIndex interface {
	Search(ctx context.Context, branch string, q store.SearchQuery) ([]store.SearchResult, error)
	GetByPath(ctx context.Context, branch, path string) (*store.FactWithBody, error)
}

// ToolSessionIndex is the interface tools require for session persistence.
type ToolSessionIndex interface {
	CreateToolSession(ctx context.Context, tool, branch, pathPrefix string) (*store.ToolSession, error)
	GetToolSession(ctx context.Context, id string) (*store.ToolSession, error)
	UpdateToolSession(ctx context.Context, id, lastCommit, status string) error
	GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error)
	AddSeenPaths(ctx context.Context, sessionID string, paths []string) error
	EnqueuePaths(ctx context.Context, sessionID string, items []store.QueueItem) error
	DequeuePaths(ctx context.Context, sessionID string, limit int) ([]store.QueueItem, error)
	QueueSize(ctx context.Context, sessionID string) (int, error)
}

// PipelineIndex is the interface the MCP tools require for pipeline session management.
type PipelineIndex interface {
	CreatePipelineSession(ctx context.Context, tool, branch string) (*store.PipelineSession, error)
	GetPipelineSession(ctx context.Context, id string) (*store.PipelineSession, error)
	CompletePipelineSession(ctx context.Context, id string) error
	InsertPipelineWorkItem(ctx context.Context, item store.PipelineWorkItem) error
	NextPipelineWorkItem(ctx context.Context, sessionID string) (*store.PipelineWorkItem, error)
	SetPipelineWorkItemResponse(ctx context.Context, id int64, response string) error
	PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error)
	GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error)
	SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error
}

// NewServer creates a new MCP server with all knomit tools registered.
// If embedder is non-nil, the learn tool uses it for batch dedup embedding.
func NewServer(gs GitStore, idx SearchIndex, sessionIdx ToolSessionIndex, pipelineIdx PipelineIndex, reviewer Reviewer, profile, ontologyRoot string, ontology *fact.Ontology, agentBranch string, embedders ...BatchEmbedder) *server.MCPServer {
	s := server.NewMCPServer("knomit", "1.0.0",
		server.WithInstructions(ProfileInstructions(profile, ontologyRoot, ontology)),
	)

	s.AddTool(learnTool(), LearnHandler(gs, idx, ontologyRoot, ontology, agentBranch, embedders...))
	s.AddTool(queryTool(), QueryHandler(gs, idx, agentBranch))
	s.AddTool(explainTool(), ExplainHandler(gs, sessionIdx, ontologyRoot, agentBranch))
	s.AddTool(updateTool(), UpdateHandler(gs, ontologyRoot, agentBranch))
	s.AddTool(exploreTool(ontologyRoot), ExploreHandler(gs, sessionIdx, ontologyRoot, agentBranch))
	s.AddTool(retractTool(), RetractHandler(gs, ontologyRoot, agentBranch))

	s.AddTool(hypothesizeTool(), HypothesizeHandler(gs, idx, pipelineIdx, ontologyRoot, agentBranch))

	if reviewer != nil {
		s.AddTool(reviewTool(), ReviewHandler(reviewer))
	}

	return s
}
