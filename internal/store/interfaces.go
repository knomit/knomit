package store

import (
	"context"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

// FactIndex is the interface for git-backed fact storage. Implemented by *Service.
type FactIndex interface {
	ReadFact(ctx context.Context, branch, path string, opts *ReadFactOpts) (ReadFactResult, error)
	WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error)
	BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFact(ctx context.Context, branch, path, message string) (string, error)
	FactExists(ctx context.Context, branch, path string) (bool, error)
	ListDir(ctx context.Context, branch, path string) ([]DirEntry, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
	ListAllWithHash(ctx context.Context, branch string) (paths []string, blobHashes []string, err error)
	Log(ctx context.Context, branch, path string) ([]LogEntry, error)
	LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error)
	CommitDetail(ctx context.Context, commitHash string) (*CommitDetailResult, error)
	Activity(ctx context.Context, branch, path string) (ActivityResult, error)
	HeadCommit(ctx context.Context, branch string) (string, error)
	LastCommitForPath(ctx context.Context, branch, path string) (string, error)
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
	WalkChangedFiles(ctx context.Context, branch, fromCommit, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error)
	FactsIter(ctx context.Context, branch string) (*FactsIter, error)
}

// SearchIndex is the interface for the fact search index. Implemented by *searchIndex.
type SearchIndex interface {
	Search(ctx context.Context, branch string, q SearchQuery) ([]SearchResult, error)
	GetByPath(ctx context.Context, branch, path string) (*FactWithBody, error)
	SyncWatermark(ctx context.Context, branch string) (string, error)
	LastCommitForPath(ctx context.Context, branch, path string) (string, bool)
	Stats(ctx context.Context, branch, pathPrefix string) (StatsResult, error)
	Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error)
	ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error)
	ClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error)
	RecentFacts(ctx context.Context, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]RecentFactEntry, int, error)
	SetEmbedder(e Embedder)
	Sync(ctx context.Context, branch string) error
	Rebuild(ctx context.Context, branch string, progress RebuildProgress) error
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

// BranchIndex is the interface for branch lifecycle operations. Implemented by *repoHandler.
type BranchIndex interface {
	EnsureBranch(ctx context.Context, name, gitRef string) (int64, error)
	MergeBranch(ctx context.Context, src, dst string) error
	DropBranch(ctx context.Context, name string) error
	ListBranches(ctx context.Context) ([]Branch, error)
	CreateBranch(ctx context.Context, branch, fromBranch string) error
	DefaultBranch(ctx context.Context) (string, error)
	SetDefaultBranch(branch string) error
	BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string)
}

// gitReader is the git-read contract implemented by repoHandler and used internally by searchIndex.
type gitReader interface {
	// DiffFiles returns paths added, modified, and deleted between fromCommit and HEAD on branch.
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
	// readFile reads the content of path from the HEAD commit of branch.
	readFile(ctx context.Context, branch, path string) (string, error)
	// readFileWithHash returns both the file content and the blob hash for the given path on branch.
	readFileWithHash(ctx context.Context, branch, path string) (content string, blobHash string, err error)
	// HeadCommit returns the hash of the current HEAD commit of branch as a hex string.
	HeadCommit(ctx context.Context, branch string) (string, error)
	// ListAll returns paths of all .md files from HEAD of branch.
	ListAll(ctx context.Context, branch string) ([]string, error)
	// ListAllWithHash returns all .md file paths and their blob hashes from HEAD of branch.
	// Single tree walk, no per-file I/O.
	ListAllWithHash(ctx context.Context, branch string) (paths []string, blobHashes []string, err error)
	// LastCommitForPath returns the hash of the most recent non-merge commit that touched path on branch.
	LastCommitForPath(ctx context.Context, branch, path string) (string, error)
	// readFileAtCommit reads the content of path at the given commit on branch.
	// branch is used for repository context; commitHash uniquely identifies the version.
	readFileAtCommit(ctx context.Context, branch, path, commitHash string) (string, error)
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

// RemoteIndex is the interface for git remote configuration and synchronization.
// Implemented by *remoteIndex, exposed on Service via Remote().
type RemoteIndex interface {
	GetRemote(name string) (*Remote, error)
	SetRemote(name, url, branch string, interval, pushInterval int, authMethod, authToken string) error
	Sync(ctx context.Context, localBranch string, auth transport.AuthMethod) (SyncResult, error)
	Push(ctx context.Context, branch string, auth transport.AuthMethod) (PushResult, error)
}
