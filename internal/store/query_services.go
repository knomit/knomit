package store

// The fact search index's read surface is carved into four cohesive query
// sub-services (P1.3 stage 2). Each holds only *repoHandler — all shared state
// and DB access live there — so none reaches sideways to another
// (invariants…/sub-index-up-not-sideways holds structurally: the only way to
// another cluster's data is UP through rh). *searchIndex keeps the index
// lifecycle (IndexManager) plus the write/rebuild/graph-write path.
//
// See .claude/plans/2026-07-21-p1.3-store-decomposition-design.md.

// factQuery implements FactQuery: fact read/search/existence queries.
type factQuery struct{ rh *repoHandler }

// graphStore implements GraphStore: DERIVED_FROM / SIMILAR_TO graph queries.
type graphStore struct{ rh *repoHandler }

// historyQuery implements HistoryQuery: commit-log / revision history queries.
type historyQuery struct{ rh *repoHandler }

// methodologyMatcher implements MethodologyMatcher: methodology-fact matching.
type methodologyMatcher struct{ rh *repoHandler }

// searchFacade composes the four sub-services into the SearchIndex composite
// returned by Service.Search(). Method promotion from the embedded pointers is
// what makes it satisfy every SearchIndex method; it holds no state of its own.
type searchFacade struct {
	*factQuery
	*graphStore
	*historyQuery
	*methodologyMatcher
}

var (
	_ FactQuery          = (*factQuery)(nil)
	_ GraphStore         = (*graphStore)(nil)
	_ HistoryQuery       = (*historyQuery)(nil)
	_ MethodologyMatcher = (*methodologyMatcher)(nil)
	_ SearchIndex        = (*searchFacade)(nil)
)
