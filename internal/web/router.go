package web

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// APIBase is the URL prefix for the API router.
const APIBase = "/api/v1"

// NewAPIRouter constructs the HAL chi router for this Server. The router
// is rooted at "/" — the caller mounts it under APIBase via chi.Mount.
// Everything below this router assumes APIBase as the Server-level prefix.
//
// Middleware order:
//  1. Recoverer — catch panics, return 500 problem+json
//  2. Compress(5) — mandatory per design spec §3 rule 6
//  3. NotFound / MethodNotAllowed handlers — return problem+json
//
// Per-route middleware (BranchMiddleware, RepoMiddleware) is attached at
// the route-group level where branches/repos appear in the path, not at
// the router root.
func (s *Server) NewAPIRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no resource at "+req.URL.Path, req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed",
			"method "+req.Method+" not allowed on "+req.URL.Path, req.URL.Path)
	})

	b := hal.URLBuilder{Base: APIBase}
	r.Get("/", handleAPIRoot(b))
	r.Get("/openapi.yaml", handleOpenAPISpec())
	r.Get("/repos", handleHALRepos(b, s.Manager))
	r.Post("/repos", handleHALReposCreate(b, s.Manager))
	r.Post("/repos:rescan", handleHALReposRescan(b, s.Manager))
	r.Get("/repos/{repo}", handleHALRepo(b, s.Manager, s.AgentBranch))

	lister := s.branchesLister
	if lister == nil {
		lister = defaultBranchesLister
	}
	r.Get("/repos/{repo}/branches", handleHALBranches(b, s.Manager, lister))

	reader := s.branchRootReader
	if reader == nil {
		reader = defaultBranchRootReader
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}",
		handleHALBranch(b, s.Manager, reader, s.AgentBranch, s.EmbeddingsEnabled),
	)

	factReader := s.factReader
	if factReader == nil {
		factReader = defaultFactReader{}
	}
	fsp := s.factSubProvider
	if fsp == nil {
		fsp = defaultFactSubProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/facts/*",
		handleHALFact(b, s.Manager, factReader, fsp),
	)

	factWriter := s.factWriter
	if factWriter == nil {
		factWriter = defaultFactWriter{}
	}
	r.With(BranchMiddleware).Put(
		"/repos/{repo}/branches/{branch}/facts/*",
		handleFactUpdate(b, s.Manager, factWriter),
	)
	r.With(BranchMiddleware).Delete(
		"/repos/{repo}/branches/{branch}/facts/*",
		handleFactDelete(b, s.Manager, factWriter),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/commits/{sha}/facts/*",
		handleCommitAnchoredFact(b, s.Manager, factReader, fsp),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/commits/{sha}/topics",
		handleCommitAnchoredTopicNode(b, s.Manager),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/commits/{sha}/topics/*",
		handleCommitAnchoredTopicNode(b, s.Manager),
	)

	topicLister := s.topicLister
	if topicLister == nil {
		topicLister = defaultTopicLister{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/topics",
		handleTopics(b, s.Manager, s.OntologyRoot, topicLister),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/topics/*",
		handleTopicNode(b, s.Manager, s.OntologyRoot, topicLister),
	)

	sp := s.searchProvider
	if sp == nil {
		sp = defaultSearchProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/search",
		handleSearch(b, s.Manager, sp, s.Embedder),
	)

	ap := s.activityProvider
	if ap == nil {
		ap = defaultActivityProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/activity",
		handleHALActivity(b, s.Manager, ap),
	)

	fcp := s.factsCollectionProvider
	if fcp == nil {
		fcp = defaultFactsCollectionProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/facts",
		handleHALFactsCollection(b, s.Manager, fcp),
	)
	r.With(BranchMiddleware).Post(
		"/repos/{repo}/branches/{branch}/facts",
		handleFactCreate(b, s.Manager, s.OntologyRoot, factWriter),
	)

	cop := s.completionsProvider
	if cop == nil {
		cop = defaultCompletionsProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/completions",
		handleHALCompletions(b, s.Manager, cop),
	)

	dp := s.domainsProvider
	if dp == nil {
		dp = defaultDomainsProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/domains",
		handleHALDomains(b, s.Manager, dp),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/domains/{name}",
		handleHALDomainFacts(b, s.Manager, dp),
	)

	sp2 := s.statsProvider
	if sp2 == nil {
		sp2 = defaultStatsProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/stats",
		handleHALStats(b, s.Manager, sp2),
	)

	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/events",
		handleHALEvents(s.Manager),
	)

	r.With(BranchMiddleware).Post(
		"/repos/{repo}/branches/{branch}/synthesis-runs",
		handleStartSynthesis(s.Manager, s.LLMAdapter),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/synthesis-runs",
		handleListJobs(s.JobRegistry, "synthesis-run"),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/synthesis-runs/{id}",
		handleGetJob(s.JobRegistry),
	)
	r.With(BranchMiddleware).Delete(
		"/repos/{repo}/branches/{branch}/synthesis-runs/{id}",
		handleDeleteJob(s.JobRegistry),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/synthesis-runs/{id}/events",
		handleJobEvents(s.Manager, s.JobRegistry),
	)
	r.With(BranchMiddleware).Post(
		"/repos/{repo}/branches/{branch}/index-rebuilds",
		handleStartRebuild(s.Manager),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/index-rebuilds",
		handleListJobs(s.JobRegistry, "index-rebuild"),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/index-rebuilds/{id}",
		handleGetJob(s.JobRegistry),
	)
	r.With(BranchMiddleware).Delete(
		"/repos/{repo}/branches/{branch}/index-rebuilds/{id}",
		handleDeleteJob(s.JobRegistry),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/index-rebuilds/{id}/events",
		handleJobEvents(s.Manager, s.JobRegistry),
	)

	mcpDispatch := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		profile := req.URL.Query().Get("profile")
		if profile == "" {
			profile = "code"
		}
		h, ok := s.mcpHandlers[profile]
		if !ok {
			h = s.mcpHandlers["code"]
		}
		if h == nil {
			http.NotFound(w, req)
			return
		}
		mw := &mcpStatusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		log.Info().
			Str("method", req.Method).
			Str("path", req.URL.Path).
			Str("ua", req.Header.Get("User-Agent")).
			Str("session", req.Header.Get("Mcp-Session-Id")).
			Msg("mcp: request in")
		h.ServeHTTP(mw, req)
		log.Info().
			Str("method", req.Method).
			Str("path", req.URL.Path).
			Int("status", mw.status).
			Str("response_content_type", mw.Header().Get("Content-Type")).
			Dur("elapsed", time.Since(start)).
			Msg("mcp: request done")
	})
	r.With(BranchMiddleware, repos.RepoMiddleware(s.Manager)).HandleFunc(
		"/repos/{repo}/branches/{branch}/mcp",
		mcpDispatch.ServeHTTP,
	)
	r.With(BranchMiddleware, repos.RepoMiddleware(s.Manager)).HandleFunc(
		"/repos/{repo}/branches/{branch}/mcp/*",
		mcpDispatch.ServeHTTP,
	)

	cp := s.commitsProvider
	if cp == nil {
		cp = defaultCommitsProvider{}
	}
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/commits",
		handleHALCommitsList(b, s.Manager, cp, s.OntologyRoot),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/commits/{sha}",
		handleHALCommitDetail(b, s.Manager, cp, s.OntologyRoot),
	)

	op := s.originProvider
	if op == nil {
		op = defaultOriginProvider{}
	}
	r.Get("/repos/{repo}/origin", handleHALGetOrigin(b, s.Manager, op))
	r.Put("/repos/{repo}/origin", handleHALSetOrigin(b, s.Manager, op))
	r.Delete("/repos/{repo}/origin", handleHALDeleteOrigin(b, s.Manager, op))

	r.Route("/repos/{repo}/origin-sessions", func(sub chi.Router) {
		sub.Use(repos.RepoMiddleware(s.Manager))
		sub.Get("/", handleListSessions(s.Manager, s.SessionManager))
		sub.Post("/", handleCreateSession(s.Manager, s.SessionManager))
		sub.Get("/{sessionID}", handleGetSession(s.Manager, s.SessionManager))
		sub.Delete("/{sessionID}", handleDeleteSession(s.Manager, s.SessionManager))
		sub.Get("/{sessionID}/test", handleTestConnectivity(s.Manager, s.SessionManager, s.AgentBranch))
		sub.Get("/{sessionID}/preview", handlePreview(s.Manager, s.SessionManager, s.AgentBranch))
		sub.Post("/{sessionID}/apply", handleApply(s.Manager, s.SessionManager, s.AgentBranch))
		sub.Post("/{sessionID}/commit", s.handleCommit(s.Manager, s.SessionManager, s.AgentBranch))
	})

	return r
}

// mcpStatusRecorder wraps http.ResponseWriter to capture the final status
// code for MCP request logging. It preserves http.Flusher so SSE streaming
// from mcp-go's streamable HTTP transport still works.
type mcpStatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (m *mcpStatusRecorder) WriteHeader(code int) {
	if !m.wroteHeader {
		m.status = code
		m.wroteHeader = true
	}
	m.ResponseWriter.WriteHeader(code)
}

func (m *mcpStatusRecorder) Write(b []byte) (int, error) {
	if !m.wroteHeader {
		m.wroteHeader = true
	}
	return m.ResponseWriter.Write(b)
}

func (m *mcpStatusRecorder) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
