// Package index is the store's index cluster: the search index (the
// IndexManager lifecycle — Sync, Rebuild, NeedsRebuild, GC — the
// write/rebuild/graph-write path and the code-managed vec tables) and the
// read-side query services carved out of it (FactQuery, GraphStore,
// HistoryQuery, MethodologyMatcher, composed by the stateless Facade), plus
// the graph SQL they share. It sits above sqlite, vcs and facts in the
// store's child order and reads a store only through *vcs.Repo; the vocab
// cluster and the facade reach up to it, never the reverse.
package index

import (
	"context"

	"knomit/internal/store/internal/vcs"
)

// New returns the search index over rh. Every sub-index of one store is built
// over the SAME *vcs.Repo, the one the parent's repoHandler wraps.
func New(rh *vcs.Repo) *Index { return &Index{rh: rh} }

// NewFactQuery returns the fact read/search/existence query service over rh.
func NewFactQuery(rh *vcs.Repo) *FactQuery { return &FactQuery{rh: rh} }

// NewGraphStore returns the DERIVED_FROM / SIMILAR_TO graph query service.
func NewGraphStore(rh *vcs.Repo) *GraphStore { return &GraphStore{rh: rh} }

// NewHistoryQuery returns the commit-log / revision history query service.
func NewHistoryQuery(rh *vcs.Repo) *HistoryQuery { return &HistoryQuery{rh: rh} }

// NewMethodologyMatcher returns the methodology-fact matching service.
func NewMethodologyMatcher(rh *vcs.Repo) *MethodologyMatcher { return &MethodologyMatcher{rh: rh} }

// NewFacade composes the four query services into the search composite returned by Service.Search().
func NewFacade(fq *FactQuery, gq *GraphStore, hq *HistoryQuery, mq *MethodologyMatcher) *Facade {
	return &Facade{FactQuery: fq, GraphStore: gq, HistoryQuery: hq, MethodologyMatcher: mq}
}

// EnsureFactsVecDefault is ensureFactsVecDefault: store.Open calls it once,
// before any embedder is configured.
func (si *Index) EnsureFactsVecDefault(ctx context.Context) error {
	return si.ensureFactsVecDefault(ctx)
}

// SetSyncSuspended sets the flag that makes Sync a no-op. Replay toggles it
// around a bulk replay into a TRANSIENT store only; see the field's comment.
func (si *Index) SetSyncSuspended(v bool) { si.syncSuspended.Store(v) }

// PersistedEmbedModelID is persistedEmbedModelID (meta.embed_model_id, "" if
// unset), for the integrity report.
func (si *Index) PersistedEmbedModelID(ctx context.Context) (string, error) {
	return si.persistedEmbedModelID(ctx)
}

// PersistedEmbedDim is persistedEmbedDim (meta.embed_dim, 0 if unset), for the
// integrity report.
func (si *Index) PersistedEmbedDim(ctx context.Context) (int, error) {
	return si.persistedEmbedDim(ctx)
}
