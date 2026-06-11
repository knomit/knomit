package store

import (
	"context"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"knomit/internal/retrieval"
)

// FactIndex is the interface for fact storage. Implemented by *factIndex.
type FactIndex interface {
	ReadFact(ctx context.Context, branch, path string, opts *ReadFactOpts) (ReadFactResult, error)
	WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error)
	BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFact(ctx context.Context, branch, path, message string) (string, error)
	FactExists(ctx context.Context, branch, path string) (bool, error)
	ListDir(ctx context.Context, branch, path string) ([]DirEntry, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
	ListAllWithHash(ctx context.Context, branch string) (paths []string, blobHashes []string, err error)
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
}

//go:generate go run go.uber.org/mock/mockgen -destination=../synthesize/mock_search_index_test.go -package=synthesize knomit/internal/store SearchIndex

// SearchIndex is the interface for querying the fact search index. Implemented by *searchIndex.
type SearchIndex interface {
	Search(ctx context.Context, branch string, q SearchOptions) ([]SearchResult, error)
	GetByPath(ctx context.Context, branch, path string) (*FactWithBody, error)
	LastCommitForPath(ctx context.Context, branch, path string) (string, bool)
	Stats(ctx context.Context, branch, pathPrefix string) (StatsResult, error)
	Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error)
	ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error)
	IncomingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error)
	OutgoingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error)
	// FactExistsAt reports whether `path` has any valid (added/modified)
	// version in the sparse history reachable from `commit` on `branch`,
	// walking past retractions. Pass commit == "" for a HEAD-anchored check.
	// Used by ref-kind classification: a ref is `fact` (vs `broken`) when
	// the target has any historical version visible at the source's anchor.
	FactExistsAt(ctx context.Context, branch, path, commit string) (bool, error)
	// FactLiveAtCommit reports whether `path` is live (present, not retracted)
	// as of `commit` — the delete-RESPECTING sibling of FactExistsAt. It
	// inspects the most recent commit_log event in the first-parent ancestry
	// and is live only if that event is added/modified. Used as the existence
	// gate for the commit-anchored /incoming and /outgoing sub-resources so a
	// retracted fact 404s in lockstep with the (no-fallback) fact read.
	FactLiveAtCommit(ctx context.Context, branch, path, commit string) (bool, error)
	RelevantMethodologyForFact(ctx context.Context, branch, factPath string, sourceDomains, sourceEntities []string, k int, minScore float64) ([]MethodologyMatch, error)
	ClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error)
	CachedClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (ClusterResult, error)
	// ClusterRefreshInFlight reports whether an async cluster-cache refresh
	// for the key is currently running. The background checker consults this
	// to skip re-dispatching a refresh (and re-logging) every tick while a
	// long Louvain compute is already in flight for the same key.
	ClusterRefreshInFlight(branch string, resolution float64, minCommunitySize int) bool
	RecentFacts(ctx context.Context, branch string, opts SearchOptions) ([]RecentFactEntry, int, error)
	Log(ctx context.Context, branch, path string) ([]LogEntry, error)
	LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error)
	// RevisionsBefore returns up to `limit` revisions of `path` in the
	// first-parent ancestry of `anchorCommit`, newest → oldest. Used by
	// knomit_explain to build the root fact's bounded evolution history.
	RevisionsBefore(ctx context.Context, branch, path, anchorCommit string, limit int) ([]RevisionMeta, error)
	CommitDetail(ctx context.Context, commitHash, pathPrefix string) (*CommitDetailResult, error)
	Activity(ctx context.Context, branch, path string) (ActivityResult, error)
	WalkChangedFiles(ctx context.Context, branch, fromCommit, prefix string, seen map[string]bool, limit int) ([]FileRecency, string, error)
	FactsIter(ctx context.Context, branch string) (*FactsIter, error)
}

// IndexManager is the interface for search index lifecycle operations. Implemented by *searchIndex.
type IndexManager interface {
	Sync(ctx context.Context, branch string) error
	Rebuild(ctx context.Context, branch string, progress RebuildProgress) error
	SyncWatermark(ctx context.Context, branch string) (string, error)
	// NeedsRebuild reports whether persisted derived state was written by an
	// older schema version and must be regenerated via Rebuild.
	NeedsRebuild(ctx context.Context) (bool, error)
	// MarkRebuildNeeded clears the persisted schema version so the next
	// NeedsRebuild reports stale. Used to undo a premature version bump after a
	// partially-failed multi-branch heal.
	MarkRebuildNeeded(ctx context.Context) error
}

// RemoteIndex is the interface for git remote configuration and synchronization.
// Implemented by *remoteIndex, exposed on Service via Remote().
type RemoteIndex interface {
	GetRemote(name string) (*Remote, error)
	SetRemote(name, url, upstreamMain, agentBranch string, interval, pushInterval int, authMethod, authToken string) error
	Sync(ctx context.Context, localBranch string, auth transport.AuthMethod) (SyncResult, error)
	Push(ctx context.Context, branch string, auth transport.AuthMethod) (PushResult, error)
}

// BranchIndex is the interface for branch lifecycle operations. Implemented by *repoHandler.
type BranchIndex interface {
	EnsureBranch(ctx context.Context, name, gitRef string) (int64, error)
	MergeBranch(ctx context.Context, src, dst string, strategy ConflictStrategy) error
	DropBranch(ctx context.Context, name string) error
	ListBranches(ctx context.Context) ([]Branch, error)
	CreateBranch(ctx context.Context, branch, fromBranch string) error
	DefaultBranch(ctx context.Context) (string, error)
	SetDefaultBranch(branch string) error
	BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string)
	HeadCommit(ctx context.Context, branch string) (string, error)
	HeadCommitInfo(ctx context.Context, branch string) (hash string, committedAt time.Time, err error)
}

// ToolSessionIndex is the interface for tool session persistence. Implemented by *toolIndex.
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

// PipelineIndex is the interface for pipeline session management. Implemented by *pipelineIndex.
type PipelineIndex interface {
	CreatePipelineSession(ctx context.Context, tool, branch string) (*PipelineSession, error)
	GetPipelineSession(ctx context.Context, id string) (*PipelineSession, error)
	AdvancePipelineSessionPhase(ctx context.Context, id, from, to string) (advanced bool, err error)
	CompletePipelineSession(ctx context.Context, id string) error
	InsertPipelineWorkItem(ctx context.Context, item PipelineWorkItem) error
	NextPipelineWorkItem(ctx context.Context, sessionID string) (*PipelineWorkItem, error)
	SetPipelineWorkItemResponse(ctx context.Context, id int64, response string) error
	PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error)
	GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error)
	SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error
}

// Embedder computes vector embeddings. Roles differ because retrieval models
// embed queries and documents with different prompts.
type Embedder interface {
	EmbedQuery(text string) ([]float32, error)
	EmbedDocument(title, body string) ([]float32, error)
	Dim() int
	ID() string
	// Thresholds returns the model's calibrated cosine cutoffs (dedup, search
	// recall, SIMILAR_TO, reflect novelty). They are model-dependent, so they
	// travel with the embedder rather than living as hard-coded constants.
	Thresholds() retrieval.Thresholds
}

// EmbedderThresholds returns emb's calibrated cutoffs, or the historical
// nomic-era defaults when no embedder is configured (embeddings disabled), so
// callers get usable values without a nil check at every site.
func EmbedderThresholds(emb Embedder) retrieval.Thresholds {
	if emb == nil {
		return retrieval.Defaults()
	}
	return emb.Thresholds()
}

//go:generate go run go.uber.org/mock/mockgen -destination=mock_batch_embedder_test.go -package=store knomit/internal/store BatchEmbedder
//go:generate go run go.uber.org/mock/mockgen -destination=../mcp/mock_batch_embedder_test.go -package=mcp knomit/internal/store BatchEmbedder

// BatchEmbedder extends Embedder with batched document inference.
type BatchEmbedder interface {
	Embedder
	EmbedDocuments(titles, bodies []string) ([][]float32, error)
}
