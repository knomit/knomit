package index

import (
	"sync/atomic"

	"knomit/internal/engine/store/internal/vcs"
)

// The fact search index's read surface is carved into four cohesive query
// sub-services (P1.3 stage 2). Each holds only the *vcs.Repo — all shared
// state and DB access live there — so none reaches sideways to another
// (invariants…/sub-index-up-not-sideways holds structurally: the only way to
// another cluster's data is UP through rh). *Index keeps the index
// lifecycle (IndexManager) plus the write/rebuild/graph-write path.
//
// See .claude/plans/2026-07-21-p1.3-store-decomposition-design.md.

// FactQuery is the fact read/search/existence queries.
type FactQuery struct {
	rh *vcs.Repo
	ov atomic.Pointer[Overrides]
}

// GraphStore is the DERIVED_FROM / SIMILAR_TO graph queries.
type GraphStore struct {
	rh *vcs.Repo
	ov atomic.Pointer[Overrides]
}

// HistoryQuery is the commit-log / revision history queries.
type HistoryQuery struct {
	rh *vcs.Repo
	ov atomic.Pointer[Overrides]
}

// MethodologyMatcher is methodology-fact matching.
type MethodologyMatcher struct {
	rh *vcs.Repo
	ov atomic.Pointer[Overrides]
}

// Facade composes the four sub-services into the search composite
// returned by Service.Search(). Method promotion from the embedded pointers is
// what makes it answer every query method; it holds no state of its own.
type Facade struct {
	*FactQuery
	*GraphStore
	*HistoryQuery
	*MethodologyMatcher
}

// probe reports a read of the named method to the test-only override set
// (SetOverridesForTest) and returns the error the test chose to inject, if any.
// With no override installed (always, in production) it is one atomic load.
func (fq *FactQuery) probe(m string) error { return probeOf(fq.ov.Load(), m) }

func (gs *GraphStore) probe(m string) error { return probeOf(gs.ov.Load(), m) }

func (hq *HistoryQuery) probe(m string) error { return probeOf(hq.ov.Load(), m) }

func (mm *MethodologyMatcher) probe(m string) error { return probeOf(mm.ov.Load(), m) }

func probeOf(o *Overrides, m string) error {
	if o == nil || o.Reads == nil {
		return nil
	}
	return o.Reads(m)
}
