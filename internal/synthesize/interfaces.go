package synthesize

import "knomit/internal/store"

// GitStore is the interface that the synthesize package requires from the git store.
type GitStore interface {
	ReadFile(path string) (string, error)
	WriteFile(path, content, message, operation string) (commitHash, blobHash string, err error)
	BatchWrite(files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFile(path, message, operation string) (commitHash string, err error)
	ListAll() ([]string, error)
	Branch() string
	DiffFiles(fromCommit string) (added, modified, deleted []string, err error)
	HeadCommit() (string, error)
}

// SearchIndex is the interface that the synthesize package requires from the search index.
type SearchIndex interface {
	Search(q store.SearchQuery) ([]store.SearchResult, error)
	Upsert(r store.FactRecord) error
	Delete(path string) error
	ClusterFacts(resolution float64, minCommunitySize int) (store.ClusterResult, error)
	GraphAddDerivedFrom(newPath string, sourcePaths []string) error
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
