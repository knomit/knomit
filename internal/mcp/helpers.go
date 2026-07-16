package mcp

import (
	"context"

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

// boundBranch returns the branch this request is bound to: the {branch} URL
// segment stashed by web.BranchMiddleware (via repos.WithBranch), falling
// back to the repo's agent branch when no branch is bound (direct handler
// calls in tests, non-branch-scoped callers).
func boundBranch(ctx context.Context, ri *repos.RepoInstance) string {
	if b, ok := repos.BranchFromContextOpt(ctx); ok && b != "" {
		return b
	}
	return ri.AgentBranch()
}
