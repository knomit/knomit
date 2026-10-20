package vcs

import (
	"context"
	"knomit/internal/embeddings/params"
)

// IndexManager is the interface for search index lifecycle operations. Implemented by *searchIndex.
type IndexManager interface {
	// Sync is lock-FREE: the caller MUST already hold LockBranch(branch). Its
	// only such caller is NotifyCommit (the inline write path). Out-of-band
	// callers (commit observer, startup heal) MUST use SyncLocked instead.
	Sync(ctx context.Context, branch string) error
	// SyncLocked runs Sync while acquiring LockBranch(branch), for callers that
	// are NOT already inside the branch lock — so the index mutation can't race
	// an inline write's sync or a concurrent Rebuild on the same branch.
	SyncLocked(ctx context.Context, branch string) error
	// Rebuild acquires LockBranch(branch) for its full duration; it is safe to
	// call out-of-band (no caller holds the branch lock first).
	Rebuild(ctx context.Context, branch string, progress RebuildProgress) error
	SyncWatermark(ctx context.Context, branch string) (string, error)
	// NeedsRebuild reports whether this BRANCH's persisted derived state was
	// written by an older schema version and must be regenerated via Rebuild.
	// Per-branch: Rebuild bumps only the branch it rebuilt, so one branch's
	// answer never speaks for another's.
	NeedsRebuild(ctx context.Context, branch string) (bool, error)
}

// Embedder computes vector embeddings. Roles differ because retrieval models
// embed queries and documents with different prompts.
//
// On ctx and what it does NOT promise: the in-process ONNX implementation runs
// inference through a session handle that exposes no run-termination hook, so
// ctx is observed only at entry to each call — and, in EmbedDocuments, between
// batches. An inference already in flight runs to completion regardless of
// cancellation; cancelling bounds latency to one batch, it does not abort one.
// The parameter is here so the interface is honest about being a cancellation
// checkpoint and so a future remote embedder (which genuinely could abort a
// request) needs no signature change — not because this implementation can
// interrupt itself.
//
// Note "one batch" is not a fixed quantity: the in-process implementation packs
// batches to a padded-token budget (embeddings.max_batch_tokens), so this
// latency bound scales with that setting and with the length of the documents
// in flight.
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocument(ctx context.Context, title, body string) ([]float32, error)
	Dim() int
	ID() string
	// Thresholds returns the model's calibrated cosine cutoffs (dedup, search
	// recall, SIMILAR_TO, reflect novelty). They are model-dependent, so they
	// travel with the embedder rather than living as hard-coded constants.
	Thresholds() params.Thresholds
}

// EmbedderThresholds returns emb's calibrated cutoffs, or the historical
// nomic-era defaults when no embedder is configured (embeddings disabled), so
// callers get usable values without a nil check at every site.
func EmbedderThresholds(emb Embedder) params.Thresholds {
	if emb == nil {
		return params.Defaults()
	}
	return emb.Thresholds()
}

// BatchEmbedder extends Embedder with batched document inference. This is the
// one embed path where ctx buys something material: a full-corpus re-embed
// issues many inferences, and the per-batch checkpoint bounds cancellation
// latency to a single batch rather than the whole corpus.
type BatchEmbedder interface {
	Embedder
	EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error)
	// EmbedShortStrings embeds bare short strings (fact titles today, motif
	// names later) through the model's short-string template. Separate from
	// EmbedDocuments because the RENDERING differs, not the batching: a few
	// words in the document template embed measurably worse than the same words
	// in the title slot (see embeddings.Model.ShortStringTemplate).
	EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error)
}

// RebuildProgress is called during Rebuild to report progress.
type RebuildProgress func(phase string, done, total int)
