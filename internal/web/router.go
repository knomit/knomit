package web

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

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
	r.Use(middleware.Recoverer)                    // produces the 500 response
	r.Use(reportPanic)                             // captures a crash bundle, re-panics
	r.Use(metricsMiddleware(nil, s.SlowRequestMS)) // nil → metrics.Default
	r.Use(middleware.Compress(5))
	if s.ReadOnly {
		r.Use(readOnlyGate)
	}

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no resource at "+req.URL.Path, req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed",
			"method "+req.Method+" not allowed on "+req.URL.Path, req.URL.Path)
	})

	// Materialize the data-access seams once. Every route below reads from
	// p; nothing re-checks for nil. Value semantics mean s.providers is
	// untouched, so a caller may build the router more than once.
	p := s.providers.withDefaults()

	b := hal.URLBuilder{Base: APIBase}

	// mcpDispatch is shared by the repo-scoped and lens-scoped MCP mounts.
	// Defined before the route tree because both subtrees close over it.
	mcpDispatch := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if profile := req.URL.Query().Get("profile"); profile != "" {
			log.Debug().Str("profile", profile).Msg("mcp: ?profile= is deprecated and ignored; profile is a per-repo setting")
		}
		h := s.mcpHandler
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

	r.Get("/", handleAPIRoot(b))
	r.Get("/version", handleVersion(b, s.ReadOnly))
	r.Get("/openapi.yaml", handleOpenAPISpec())
	r.Get("/archived", handleHALArchived(b, s.Manager))
	r.Post("/archived/{id}/restore", handleHALArchivedRestore(b, s.Manager))
	r.Delete("/archived/{id}", handleHALArchivedPurge(s.Manager))

	// Repo collection. These carry no {repo} segment, so they resolve their
	// own arguments and are never wrapped by RepoMiddleware.
	//
	// By convention, routes with a ':' action suffix (e.g. a future
	// "/repos:rehydrate") are registered up here, before "/repos/{repo}" —
	// though chi's radix tree does not actually depend on that order. It
	// treats only "{...}" and "*" as special, so such a route diverges from
	// "/repos/..." at the ':' and never enters the {repo} param edge either
	// way.
	r.Get("/repos", handleHALRepos(b, s.Manager))
	r.Post("/repos", handleHALReposCreate(b, s.Manager))

	r.Route("/repos/{repo}", func(r chi.Router) {
		// Archive deliberately sits OUTSIDE the middleware group: it resolves
		// through m.Archive, not m.Get, and archiveErrStatus attributes its
		// own not-found/conflict errors. Wrapping it would replace that
		// attribution with a generic middleware 404. Registered with the "/"
		// pattern inside the Route rather than flat alongside it, which would
		// risk a chi Mount conflict.
		r.Delete("/", handleHALRepoArchive(b, s.Manager))

		// Everything below resolves {repo} exactly once, in the middleware.
		// Handlers read the instance with repos.RepoFromContext.
		r.Group(func(r chi.Router) {
			r.Use(RepoMiddleware(s.Manager))

			r.Get("/", handleHALRepo(b))
			r.Patch("/", handleHALRepoPatch(b))

			r.Get("/origin", handleHALGetOrigin(b, p.origin))
			r.Put("/origin", handleHALSetOrigin(b, s.Manager, p.origin))
			r.Patch("/origin/upstream", handleHALSetOriginUpstream(b, s.Manager, p.origin))
			r.Delete("/origin", handleHALDeleteOrigin(b, s.Manager, p.origin))

			r.Route("/origin-sessions", func(r chi.Router) {
				r.Get("/", handleListSessions(b, s.SessionManager))
				r.Post("/", handleCreateSession(b, s.SessionManager))
				r.Get("/{sessionID}", handleGetSession(b, s.SessionManager))
				r.Delete("/{sessionID}", handleDeleteSession(s.SessionManager))
				r.Get("/{sessionID}/test", handleTestConnectivity(s.Manager, s.SessionManager, s.AgentBranch))
				r.Get("/{sessionID}/preview", handlePreview(s.SessionManager, s.AgentBranch))
				r.Post("/{sessionID}/apply", handleApply(s.SessionManager, s.AgentBranch))
				r.Post("/{sessionID}/commit", s.handleCommit(s.Manager, s.SessionManager, s.AgentBranch))
			})

			// Branch list carries no {branch} segment, so it stays outside
			// the BranchMiddleware subtree.
			r.Get("/branches", handleHALBranches(b, p.branchesLister))

			r.Route("/branches/{branch}", func(r chi.Router) {
				r.Use(BranchMiddleware)

				r.Get("/", handleHALBranch(b, p.branchRootReader, s.AgentBranch, s.EmbeddingsEnabled))

				r.Get("/facts", handleHALFactsCollection(b, p.factsCollection))
				r.Post("/facts", handleFactCreate(b, s.OntologyRoot, p.factWriter))
				r.Get("/facts/*", handleHALFact(b, p.factReader, p.factSub))
				r.Put("/facts/*", handleFactUpdate(b, p.factWriter))
				r.Delete("/facts/*", handleFactDelete(b, p.factWriter))

				r.Get("/topics", handleTopics(b, s.OntologyRoot, p.topicLister))
				r.Get("/topics/*", handleTopicNode(b, s.OntologyRoot, p.topicLister))

				r.Get("/commits", handleHALCommitsList(b, p.commits, s.OntologyRoot))
				r.Get("/commits/{sha}", handleHALCommitDetail(b, p.commits, s.OntologyRoot))
				r.Get("/commits/{sha}/facts/*", handleCommitAnchoredFact(b, p.factReader, p.factSub))
				r.Get("/commits/{sha}/topics", handleCommitAnchoredTopicNode())
				r.Get("/commits/{sha}/topics/*", handleCommitAnchoredTopicNode())

				r.Get("/search", handleSearch(b, p.search, s.Embedder))
				r.Get("/activity", handleHALActivity(b, p.activity))
				r.Get("/completions", handleHALCompletions(b, p.completions))
				r.Get("/domains", handleHALDomains(b, p.domains))
				r.Get("/domains/{name}", handleHALDomainFacts(b, p.domains))
				r.Get("/stats", handleHALStats(b, p.stats))
				r.Get("/events", handleHALEvents())

				r.Post("/synthesis-runs", handleStartSynthesis(s.LLMAdapter))
				r.Get("/synthesis-runs", handleListJobs(s.JobRegistry, "synthesis-run"))
				r.Get("/synthesis-runs/{id}", handleGetJob(s.JobRegistry))
				r.Delete("/synthesis-runs/{id}", handleDeleteJob(s.JobRegistry))
				r.Get("/synthesis-runs/{id}/events", handleJobEvents(s.JobRegistry))

				r.Post("/index-rebuilds", handleStartRebuild())
				r.Get("/index-rebuilds", handleListJobs(s.JobRegistry, "index-rebuild"))
				r.Get("/index-rebuilds/{id}", handleGetJob(s.JobRegistry))
				r.Delete("/index-rebuilds/{id}", handleDeleteJob(s.JobRegistry))
				r.Get("/index-rebuilds/{id}/events", handleJobEvents(s.JobRegistry))

				r.HandleFunc("/mcp", mcpDispatch.ServeHTTP)
				r.HandleFunc("/mcp/*", mcpDispatch.ServeHTTP)
			})
		})
	})

	// Lens collection: no {lens} segment, so unwrapped.
	r.Get("/lenses", handleHALLenses(b, s.Manager))
	r.Post("/lenses", handleHALLensesCreate(b, s.Manager))

	r.Route("/lenses/{lens}", func(r chi.Router) {
		// The lens CRUD trio resolves through the registry directly and
		// reports its own errors, so it stays outside the binding group.
		r.Get("/", handleHALLens(b, s.Manager))
		r.Patch("/", handleHALLensPatch(b, s.Manager))
		r.Delete("/", handleHALLensDelete(s.Manager))

		r.Group(func(r chi.Router) {
			r.Use(LensMiddleware(s.Manager))
			r.Get("/facts", handleHALLensFacts(p.factsCollection))
			r.Get("/facts/*", handleHALLensFact(b, p.factReader))
			r.Get("/search", handleHALLensSearch(p.search, s.Embedder))
			r.Get("/completions", handleHALLensCompletions(p.completions))
			r.Get("/stats", handleHALLensStats(p.stats, p.activity))
			r.Get("/topics", handleHALLensTopics(p.topicLister, s.OntologyRoot))
			r.Get("/topics/*", handleHALLensTopics(p.topicLister, s.OntologyRoot))
			r.HandleFunc("/mcp", mcpDispatch.ServeHTTP)
			r.HandleFunc("/mcp/*", mcpDispatch.ServeHTTP)
		})
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
