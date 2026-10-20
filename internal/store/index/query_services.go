package index

import "knomit/internal/store/internal/vcs"

// The fact search index's read surface is carved into four cohesive query
// sub-services (P1.3 stage 2). Each holds only the *vcs.Repo — all shared
// state and DB access live there — so none reaches sideways to another
// (invariants…/sub-index-up-not-sideways holds structurally: the only way to
// another cluster's data is UP through rh). *Index keeps the index
// lifecycle (IndexManager) plus the write/rebuild/graph-write path.
//
// See .claude/plans/2026-07-21-p1.3-store-decomposition-design.md.

// FactQuery implements store.FactQuery: fact read/search/existence queries.
type FactQuery struct{ rh *vcs.Repo }

// GraphStore implements store.GraphStore: DERIVED_FROM / SIMILAR_TO graph queries.
type GraphStore struct{ rh *vcs.Repo }

// HistoryQuery implements store.HistoryQuery: commit-log / revision history queries.
type HistoryQuery struct{ rh *vcs.Repo }

// MethodologyMatcher implements store.MethodologyMatcher: methodology-fact matching.
type MethodologyMatcher struct{ rh *vcs.Repo }

// Facade composes the four sub-services into the SearchIndex composite
// returned by Service.Search(). Method promotion from the embedded pointers is
// what makes it satisfy every SearchIndex method; it holds no state of its own.
type Facade struct {
	*FactQuery
	*GraphStore
	*HistoryQuery
	*MethodologyMatcher
}
