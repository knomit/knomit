package web

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// Server holds server-wide state for the HTTP layer.
type Server struct {
	Manager           *repos.Manager
	GitHandler        http.Handler
	EmbeddingsEnabled bool
	OntologyRoot      string
	AgentBranch       string
	SessionManager    *SessionManager
	LLMAdapter        llm.LLMAdapter      // nil if no LLM configured
	Embedder          store.BatchEmbedder // nil if unavailable

	// ReadOnly runs the instance as a read-only demo: /git is not mounted,
	// MCP exposes only read tools, and the API router rejects mutations.
	ReadOnly bool

	// SlowRequestMS, when > 0, logs any HTTP request slower than this many
	// milliseconds at WARN. Wired from config ([log].slow_request_ms).
	SlowRequestMS int

	// APIOnly omits the embedded web UI routes (SPA + /assets). The desktop
	// build sets this; the UI is served in-process by Wails. Unknown routes
	// then return an API-consistent problem+json 404. Zero value (false) keeps
	// the cloud default of serving the UI.
	APIOnly bool
	// CORSOrigins is the allow-list for cross-origin browser requests (the
	// Wails origin in the desktop build, e.g. "wails://localhost"). Empty means
	// no CORS headers are emitted (cloud default).
	CORSOrigins []string

	mcpHandler http.Handler // single MCP server; profile is a per-repo setting

	JobRegistry *JobRegistry // tracks synthesis-run and index-rebuild jobs

	// providers holds the test-injectable data-access seams the API router
	// wires into handlers. The zero value means "all production defaults";
	// NewAPIRouter materializes them via withDefaults(). Tests set only the
	// members they stub. See storeProviders in providers.go.
	providers storeProviders
}

// buildMCPHandler constructs the single MCP server instance, shared across
// all repos and lenses. Profile is a per-repo attribute now; the formerly
// profile-keyed instances are collapsed (lenses RFC decision 12).
func (s *Server) buildMCPHandler() {
	var mcpSrv *mcpserver.MCPServer
	if s.Embedder != nil {
		mcpSrv = mcp.NewServer(s.OntologyRoot, s.Manager, s.ReadOnly, s.Embedder)
	} else {
		mcpSrv = mcp.NewServer(s.OntologyRoot, s.Manager, s.ReadOnly)
	}
	s.mcpHandler = mcpserver.NewStreamableHTTPServer(mcpSrv)
}

// Handler returns the chi router with all routes mounted.
func (s *Server) Handler() http.Handler {
	if s.mcpHandler == nil {
		s.buildMCPHandler()
	}
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	if len(s.CORSOrigins) > 0 {
		r.Use(corsMiddleware(s.CORSOrigins))
	}
	if s.GitHandler != nil && !s.ReadOnly {
		log.Info().Msg("git handler enabled at /git")
		r.Mount("/git", s.GitHandler)
	}

	r.Get("/docs", handleSwaggerUI())

	// Mount the API router.
	r.Mount(APIBase, s.NewAPIRouter())

	if s.APIOnly {
		// Pure API server (desktop build serves the UI in-process via Wails).
		// Unknown routes return an API-consistent problem+json 404.
		r.NotFound(func(w http.ResponseWriter, req *http.Request) {
			hal.WriteProblem(w, http.StatusNotFound, "Not Found",
				"no resource at "+req.URL.Path, req.URL.Path)
		})
	} else {
		// Serve embedded web UI.
		staticHandler := StaticHandler()
		r.Handle("/assets/*", staticHandler)
		r.Get("/*", newSPAHandler(staticHandler))
	}

	return r
}

// defaultBranchesLister reads the branch list from a repo's store via
// WithRead. Returns nil, nil if the store is unavailable (e.g. the repo is
// still opening) so handlers can distinguish "no data yet" from "error".
func defaultBranchesLister(ctx context.Context, ri *repos.RepoInstance) ([]store.Branch, error) {
	var (
		out []store.Branch
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Branches().ListBranches(ctx)
	})
	return out, err
}

// defaultBranchRootReader reads head + index watermark via the store. Plan
// 02 may expand this to include more fields (branch metadata, last commit
// time) as those handlers come online.
func defaultBranchRootReader(ctx context.Context, ri *repos.RepoInstance, branch string) (branchRootInfo, error) {
	var (
		info branchRootInfo
		err  error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		head, herr := svc.Branches().HeadCommit(ctx, branch)
		if herr != nil {
			err = herr
			return
		}
		info.Head = head
		idx, ierr := svc.IndexManager().SyncWatermark(ctx, branch)
		if ierr == nil {
			info.IndexCommit = idx
		}
	})
	return info, err
}
