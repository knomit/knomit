package web

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"knomit/internal/client/sessions"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/platform/logging"
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
	// ClientSessions records every MCP request's session (control.db
	// client_sessions). nil ⇒ recording is off (tests, degraded boot). Wired
	// from Manager.ClientSessions() by internal/app, AFTER Manager.Start
	// opens it.
	ClientSessions *sessions.Store
	LLMAdapter     llm.LLMAdapter      // nil if no LLM configured
	Embedder       store.BatchEmbedder // nil if unavailable

	// Logs is the in-process log tap that GET /api/v1/logs/events streams
	// from: a bounded ring of recent lines plus a live fan-out. Set by
	// internal/app from the tap each binary wires into its logging chain, and
	// nil for a server built without one — the endpoint then answers 503,
	// exactly as the sessions endpoints do without their store.
	Logs *logging.Tap

	// ReadOnly runs the instance as a read-only demo: /git is not mounted,
	// MCP exposes only read tools, and the API router rejects mutations.
	ReadOnly bool

	// SlowRequestMS, when > 0, logs any HTTP request slower than this many
	// milliseconds at WARN. Wired from config ([log].slow_request_ms).
	SlowRequestMS int

	// ExperimentExpiryDays mirrors [experiments].expiry_days. It is carried
	// here only so a branch row and the experiments collection can render
	// expires_at; 0 means experiments never expire, and then the field is
	// OMITTED rather than computed.
	ExperimentExpiryDays int

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

	// staticFS overrides the embedded web UI. nil — the production zero
	// value — means embeddedStaticFS(). Tests set it to an fstest.MapFS
	// because web/dist holds only .gitkeep until `make web` has run, and the
	// Go tests must not depend on the npm build.
	staticFS fs.FS

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
	// mcp-go does not put the *http.Request in the context it hands to hooks,
	// so the initialize hook — the only place the declared clientInfo exists —
	// would otherwise be unable to see the ip and User-Agent of the request
	// carrying it, and would derive every direct-HTTP client's identity from
	// two empty strings.
	s.mcpHandler = mcpserver.NewStreamableHTTPServer(mcpSrv,
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return sessions.WithHTTPInfo(ctx, r.RemoteAddr, r.Header.Get("User-Agent"))
		}),
	)
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

	// /docs is 8 KB of text/html and sits on the OUTER router, which carries
	// no compressor — so it needs the shared one attached per route. It stays
	// out of the static Group below on purpose: that group also carries
	// identityForRangeRequests and the cache policy, and /docs sends no cache
	// headers today. HEAD is registered too, so an uptime check that HEADs it
	// does not see a 405; net/http discards the body for HEAD itself.
	//
	// The handler and the compressor are hoisted so BOTH verbs share one
	// value of each, exactly as the static routes below do. Calling
	// handleSwaggerUI() and compressor() once per verb would build two
	// closures and two Compressors, and "GET and HEAD answer identically"
	// would then be a coincidence maintained by hand rather than a property
	// of the wiring.
	docs := handleSwaggerUI()
	docsRoute := r.With(compressor())
	docsRoute.Method(http.MethodGet, "/docs", docs)
	docsRoute.Method(http.MethodHead, "/docs", docs)

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
		// Serve embedded web UI. These routes get their own compressor and
		// cache policy in a Group rather than on the outer router: the API
		// router mounted above already carries compressor(), and a second
		// one wrapping it would gzip every API body twice.
		fsys := s.staticFS
		if fsys == nil {
			fsys = embeddedStaticFS()
		}
		staticHandler := staticHandlerFor(fsys)
		tags := newETagger(fsys)
		r.Group(func(r chi.Router) {
			// identityForRangeRequests must sit ABOVE the compressor: it
			// works by hiding Accept-Encoding from it.
			r.Use(identityForRangeRequests)
			r.Use(compressor())
			// GET and HEAD on BOTH static routes. r.Handle, which this
			// used to be, registers every method — so POST to a bundle
			// answered 200 with the whole file. Nothing was mutable, it is
			// a read-only file server, but it is the same asymmetry that
			// made HEAD / a 405, pointing the other way.
			asset := newAssetHandler(fsys, staticHandler)
			r.Method(http.MethodGet, assetsPrefix+"*", asset)
			r.Method(http.MethodHead, assetsPrefix+"*", asset)
			// Same two verbs for the SPA fallback. An uptime check that
			// HEADs the root used to get a 405 and report the app down,
			// because r.Get registers exactly one method. Both verbs share
			// one handler VALUE on each route, so their headers cannot
			// diverge; http.ServeContent omits the body for HEAD itself.
			spa := newSPAHandler(fsys, staticHandler, tags)
			r.Get("/*", spa)
			r.Head("/*", spa)
		})
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
