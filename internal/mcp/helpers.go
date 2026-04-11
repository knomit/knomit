package mcp

import (
	"knomit/internal/repos"
	"knomit/internal/store"
)

// mcpStore bundles the store indices used by MCP handlers.
// Retrieved from a *repos.RepoInstance under a single read lock.
type mcpStore struct {
	facts       store.FactIndex
	search      store.SearchIndex
	toolSession store.ToolSessionIndex
	pipeline    store.PipelineIndex
	branches    store.BranchIndex
}

// storeIndices extracts the indices a handler may need from ri under a single
// read lock. Handlers use only the fields they need and ignore the rest.
func storeIndices(ri *repos.RepoInstance) mcpStore {
	var s mcpStore
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		s.facts = svc.Facts()
		s.search = svc.Search()
		s.toolSession = svc.ToolSession()
		s.pipeline = svc.Pipeline()
		s.branches = svc.Branches()
	})
	return s
}
