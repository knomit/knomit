package store

import "sync"

type searchIndex struct {
	rh       *repoHandler
	embedMu  sync.RWMutex
	embedder Embedder
}

// SetEmbedder attaches an Embedder to the index. When set, Upsert will call
// Embed on each record's body and persist the result in facts_vec.
func (si *searchIndex) SetEmbedder(e Embedder) {
	si.embedMu.Lock()
	defer si.embedMu.Unlock()
	si.embedder = e
}

// getEmbedder returns the current Embedder under a read lock.
func (si *searchIndex) getEmbedder() Embedder {
	si.embedMu.RLock()
	defer si.embedMu.RUnlock()
	return si.embedder
}
