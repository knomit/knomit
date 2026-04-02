package store

import "context"

// GitStore is the interface for git-backed fact storage. Implemented by *Service.
type GitStore interface {
	ReadFact(ctx context.Context, branch, path string, opts *ReadFactOpts) (ReadFactResult, error)
	WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error)
	BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFact(ctx context.Context, branch, path, message string) (string, error)
	FactExists(ctx context.Context, branch, path string) (bool, error)
	ListDir(ctx context.Context, branch, path string) ([]DirEntry, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
	Log(ctx context.Context, branch, path string) ([]LogEntry, error)
	LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error)
	CommitDetail(ctx context.Context, commitHash string) (*CommitDetailResult, error)
	Activity(ctx context.Context, branch, path string) (ActivityResult, error)
	HeadCommit(ctx context.Context, branch string) (string, error)
	LastCommitForPath(ctx context.Context, branch, path string) (string, error)
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
	WalkChangedFiles(ctx context.Context, branch, fromCommit, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error)
}

// SearchIndex is the interface for the fact search index. Implemented by *Index.
type SearchIndex interface {
	Search(ctx context.Context, branch string, q SearchQuery) ([]SearchResult, error)
	GetByPath(ctx context.Context, branch, path string) (*FactWithBody, error)
	GetLastCommit(ctx context.Context, branch string) (string, error)
	Upsert(ctx context.Context, branch, commitHash string, r FactRecord) error
	Delete(ctx context.Context, branch, path string) error
	Stats(ctx context.Context, branch, pathPrefix string) (StatsResult, error)
	Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error)
	ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error)
	ClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error)
}

// ToolSessionIndex is the interface for tool session persistence. Implemented by *Index.
type ToolSessionIndex interface {
	CreateToolSession(ctx context.Context, tool, branch, pathPrefix string) (*ToolSession, error)
	GetToolSession(ctx context.Context, id string) (*ToolSession, error)
	UpdateToolSession(ctx context.Context, id, lastCommit, status string) error
	GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error)
	AddSeenPaths(ctx context.Context, sessionID string, paths []string) error
	EnqueuePaths(ctx context.Context, sessionID string, items []QueueItem) error
	DequeuePaths(ctx context.Context, sessionID string, limit int) ([]QueueItem, error)
	QueueSize(ctx context.Context, sessionID string) (int, error)
}

// PipelineIndex is the interface for pipeline session management. Implemented by *Index.
type PipelineIndex interface {
	CreatePipelineSession(ctx context.Context, tool, branch string) (*PipelineSession, error)
	GetPipelineSession(ctx context.Context, id string) (*PipelineSession, error)
	CompletePipelineSession(ctx context.Context, id string) error
	InsertPipelineWorkItem(ctx context.Context, item PipelineWorkItem) error
	NextPipelineWorkItem(ctx context.Context, sessionID string) (*PipelineWorkItem, error)
	SetPipelineWorkItemResponse(ctx context.Context, id int64, response string) error
	PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error)
	GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error)
	SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error
}

// Embedder computes vector embeddings for text.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// BatchEmbedder extends Embedder with batch inference support.
type BatchEmbedder interface {
	Embedder
	EmbedBatch(texts []string) ([][]float32, error)
}
