package web

import (
	"context"
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"knomit/internal/clustercache"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Server holds server-wide state for the HTTP layer.
type Server struct {
	Manager           *repos.Manager
	GitHandler        http.Handler
	EmbeddingsEnabled bool
	OntologyRoot      string
	AgentBranch       string
	SessionManager    *SessionManager
	LLMAdapter        llm.LLMAdapter     // nil if no LLM configured
	Embedder          store.BatchEmbedder // nil if unavailable

	// ClusterCache wraps idx.ClusterFacts with an SQLite-backed cache.
	// Required by knomit_review (MCP) and the synthesis-run job; nil is
	// only valid during partial test setups.
	ClusterCache *clustercache.Cache

	mcpHandlers map[string]http.Handler // profile → handler

	JobRegistry *JobRegistry // tracks synthesis-run and index-rebuild jobs

	// branchesLister is a per-repo branch enumeration hook. Injected for
	// tests; production wires it to ri.WithRead → svc.Branches().ListBranches.
	branchesLister func(ri *repos.RepoInstance) ([]store.Branch, error)

	branchRootReader func(ri *repos.RepoInstance, branch string) (branchRootInfo, error)

	factReader FactReader

	topicLister TopicLister

	searchProvider searchProvider

	commitsProvider commitsProvider

	factSubProvider factSubProvider

	statsProvider statsProvider

	domainsProvider domainsProvider

	completionsProvider completionsProvider

	factsCollectionProvider factsCollectionProvider

	activityProvider activityProvider

	factWriter FactWriter

	originProvider originProvider
}

// buildMCPHandlers constructs one MCP server per profile, shared across all
// repos. Each handler resolves the repo from the request context at call time.
func (s *Server) buildMCPHandlers() {
	profiles := []string{"code", "chat", "generic"}
	s.mcpHandlers = make(map[string]http.Handler, len(profiles))
	for _, p := range profiles {
		var mcpSrv *mcpserver.MCPServer
		if s.Embedder != nil {
			mcpSrv = mcp.NewServer(p, s.OntologyRoot, s.ClusterCache, s.Embedder)
		} else {
			mcpSrv = mcp.NewServer(p, s.OntologyRoot, s.ClusterCache)
		}
		s.mcpHandlers[p] = mcpserver.NewStreamableHTTPServer(mcpSrv)
	}
}

// Handler returns the chi router with all routes mounted.
func (s *Server) Handler() http.Handler {
	if s.mcpHandlers == nil {
		s.buildMCPHandlers()
	}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	if s.GitHandler != nil {
		log.Info().Msg("git handler enabled at /git")
		r.Mount("/git", s.GitHandler)
	}

	r.Get("/docs", handleSwaggerUI())

	// Mount the API router.
	r.Mount(APIBase, s.NewAPIRouter())

	// Serve embedded web UI
	staticHandler := StaticHandler()
	r.Handle("/assets/*", staticHandler)
	r.Get("/*", newSPAHandler(staticHandler))

	return r
}

// defaultBranchesLister reads the branch list from a repo's store via
// WithRead. Returns nil, nil if the store is unavailable (e.g. the repo is
// still opening) so handlers can distinguish "no data yet" from "error".
func defaultBranchesLister(ri *repos.RepoInstance) ([]store.Branch, error) {
	var (
		out []store.Branch
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Branches().ListBranches(contextTODO())
	})
	return out, err
}

// defaultBranchRootReader reads head + index watermark via the store. Plan
// 02 may expand this to include more fields (branch metadata, last commit
// time) as those handlers come online.
func defaultBranchRootReader(ri *repos.RepoInstance, branch string) (branchRootInfo, error) {
	var (
		info branchRootInfo
		err  error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		head, herr := svc.Branches().HeadCommit(contextTODO(), branch)
		if herr != nil {
			err = herr
			return
		}
		info.Head = head
		idx, ierr := svc.IndexManager().SyncWatermark(contextTODO(), branch)
		if ierr == nil {
			info.IndexCommit = idx
		}
	})
	return info, err
}

// contextTODO is a placeholder for context propagation. Plan 02 replaces
// this with the request context where applicable; for now the lister is
// only called from handler scopes that already hold a request context but
// don't need cancellation for a trivial read.
func contextTODO() context.Context { return context.Background() }

