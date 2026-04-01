package synthesize

import (
	"context"

	"knomit/internal/store"
)

// GitStore is the interface that the synthesize package requires from the git store.
type GitStore interface {
	ReadFact(ctx context.Context, branch, path string, opts *store.ReadFactOpts) (store.ReadFactResult, error)
	WriteFact(ctx context.Context, branch, path, content, message, operation string) (store.WriteFactResult, error)
	BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFact(ctx context.Context, branch, path, message string) (string, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit(ctx context.Context, branch string) (string, error)
}

// SearchIndex is the interface that the synthesize package requires from the search index.
type SearchIndex interface {
	Search(ctx context.Context, branch string, q store.SearchQuery) ([]store.SearchResult, error)
	Upsert(ctx context.Context, branch, commitHash string, r store.FactRecord) error
	Delete(ctx context.Context, branch, path string) error
	ClusterFacts(ctx context.Context, branch string, resolution float64, minCommunitySize int) (store.ClusterResult, error)
}

// ProgressEvent carries progress information from the pipeline to the caller.
type ProgressEvent struct {
	Phase   string
	Message string
}

// Embedder is the interface for computing embedding vectors.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// BatchEmbedder extends Embedder with batch inference support.
type BatchEmbedder interface {
	Embedder
	EmbedBatch(texts []string) ([][]float32, error)
}
