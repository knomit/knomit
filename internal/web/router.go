package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

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
	r.Get("/repos", handleHALRepos(b, s.Manager))
	r.Get("/repos/{repo}", handleHALRepo(b, s.Manager))

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
	r.With(BranchMiddleware).Post(
		"/repos/{repo}/branches/{branch}/index-rebuilds",
		handleStartRebuild(s.Manager),
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
		h.ServeHTTP(w, req)
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
		handleHALCommitsList(b, s.Manager, cp),
	)
	r.With(BranchMiddleware).Get(
		"/repos/{repo}/branches/{branch}/commits/{sha}",
		handleHALCommitDetail(b, s.Manager, cp),
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
