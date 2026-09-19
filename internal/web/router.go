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
//  2. compressor() — the shared allowlist from compress.go. It MUST be the
//     shared one: chi's built-in list has no application/hal+json, so the
//     bare middleware.Compress(5) that stood here compressed nothing at all.
//  3. NotFound / MethodNotAllowed handlers — return problem+json
//
// Per-route middleware (BranchMiddleware, RepoMiddleware) is attached at
// the route-group level where branches/repos appear in the path, not at
// the router root.
func (s *Server) NewAPIRouter() chi.Router {
	r := chi.NewRouter()
	// Correlation id for slow-request warnings. chi echoes an inbound
	// X-Request-Id verbatim and only generates one when the header is absent, so
	// req_id is trustworthy exactly as far as the client is: useful for stitching
	// a proxy's id to ours, forgeable by anyone who wants two requests to look
	// like one. It labels log lines only — never an authorization decision.
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)                    // produces the 500 response
	r.Use(reportPanic)                             // captures a crash bundle, re-panics
	r.Use(metricsMiddleware(nil, s.SlowRequestMS)) // nil → metrics.Default
	r.Use(compressor())
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
		// AFTER the response: recording is never in the request's critical
		// path, and never a reason for it to fail.
		//
		// ALLOWLIST, not "anything but 404". mcp-go rejects a bad
		// Content-Type or an unparseable body with 400 BEFORE it resolves the
		// session id at all, so a deny-list would let any caller mint a row
		// per request under an id of their choosing. 200 (call or DELETE) and
		// 202 (notification) are the only statuses reached past session
		// resolution.
		if mw.status == http.StatusOK || mw.status == http.StatusAccepted {
			recordClientSession(req, s.ClientSessions)
		}
	})

	r.Get("/", handleAPIRoot(b, s.ReadOnly))
	r.Get("/version", handleVersion(b, s.ReadOnly))
	r.Get("/openapi.yaml", handleOpenAPISpec())
	r.Get("/sessions", handleHALClientSessions(b, s.Manager, s.ClientSessions, s.ReadOnly))
	r.Get("/sessions/events", handleHALClientSessionEvents(s.ClientSessions))
	// Server-wide REPO-LEVEL event stream. A TOP-LEVEL collection, deliberately not
	// under /repos/: see the no-static-children rule on /repo-creates below.
	// It is about every repo rather than any one, and the UI holds exactly one.
	r.Get("/repo-events", handleRepoEvents(s.Manager))
	r.Get("/logs/events", handleLogEvents(s.Logs, s.ReadOnly))
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

	// The poll target for the 202 that POST /repos answers with. A SIBLING
	// collection rather than "/repos/creates/{id}", and the precedent for a
	// rule this router now holds to: NO STATIC ROUTE SITS BESIDE A {param}
	// WHOSE VALUE A USER CAN CAUSE TO EXIST.
	//
	// Such a segment is also a legal value for that param, so it shadows the
	// thing named that. SIX subtrees qualify today, and the prohibition covers
	// all of them:
	//
	//   /repos                                    {repo}
	//   /lenses                                   {lens}
	//   /repos/{repo}/branches                    {branch}
	//   /repos/{repo}/branches/{branch}/domains   {name}
	//   /repos/{repo}/branches/{branch}/motifs    {key}
	//   /lenses/{lens}/motifs                     {key}
	//
	// Repo and lens names share one validator (repos.IsValidName); branch names
	// are chosen too; domain tags and motif keys are AUTHORED — someone writing
	// `domain: [stats]` in a fact's frontmatter creates one, so /domains/stats
	// would shadow it. The test is "can a user cause a value with that name to
	// exist", not "does a user own it": ownership tracks how bad a collision
	// would be, not whether it can happen.
	//
	// The two /motifs entries are DISTINCT parents, not one — a lens-scoped and
	// a branch-scoped view, each shadowable in its own scope. Counting by last
	// segment gives five and silently drops one.
	//
	// /ontologies/presets/{name} is deliberately NOT in the set: those are our
	// shipped preset names, which no user adds to. It shares the {name} spelling
	// with /domains/{name} and has the opposite answer, which is why the test
	// classifies by parent prefix rather than by param name.
	// The cost was measured against the vendored chi rather than assumed — an
	// earlier version of this comment blamed the absence of backtracking and
	// was wrong, in a way that mispredicted which static routes are safe:
	//
	//   GET    /repos/events           -> the static route   <- shadowed
	//   DELETE /repos/events           -> {repo}             <- falls through
	//   GET    /repos/events/branches  -> {repo}             <- falls through
	//   GET    /repos/creates          -> {repo}             <- works
	//   GET    /repos/creates/branches -> creates/{id}       <- BROKEN
	//
	// chi DOES backtrack to the {repo} param when the static edge has no
	// handler for the method or no child for the next segment. So a static
	// LEAF costs exactly one method+path, while a static segment with a PARAM
	// CHILD costs the whole subtree below it, because the greedy {id} matches
	// "branches" and wins. Neither is diagnosable by the repo's owner, and the
	// difference is invisible from "static beats param" alone — which is why
	// the rule is the blunt one rather than a judgement per route.
	//
	// TestRouter_NoStaticSiblingsOfUserNamedParams enforces it, deriving the
	// guarded prefixes from the route table rather than from a list here.
	r.Get("/repo-creates/{id}", handleHALRepoCreateStatus(b, s.Manager))
	// The collection: a client that lost the id from its 202 finds its create
	// here. DELETE forgets a FINISHED job so a failed row can be dismissed
	// without waiting out CreateJobTTL.
	r.Get("/repo-creates", handleHALRepoCreates(b, s.Manager))
	r.Delete("/repo-creates/{id}", handleHALRepoCreateDismiss(s.Manager))
	// Cancel is a POST on a colon-suffixed sub-resource, the same spelling as
	// /repos:probe-origin: it is an ACTION on the job, not a deletion of its
	// row, and the two must stay distinct because dismiss refuses a running
	// job while cancel is exactly for one.
	r.Post("/repo-creates/{id}:cancel", handleHALRepoCreateCancel(b, s.Manager))

	// Probes an origin before create, so the wizard can classify it (has refs
	// / empty / unreachable) instead of asking the user to declare that up
	// front. Collection-level for the same reason as the ontology block below:
	// it runs BEFORE any repo exists.
	r.Post("/repos:probe-origin", handleReposProbeOrigin(s.Manager))

	// The second, per-BRANCH half of that classification: does the chosen
	// branch already hold a knomit knowledge base? Separate from probe-origin
	// because the answer differs per branch and the branch is not known when
	// that probe runs — and because this one actually transfers a commit
	// (shallow, single-branch, discarded) rather than only listing refs.
	r.Post("/repos:probe-initialized", handleReposProbeInitialized(s.Manager))

	// Ontology endpoints. Collection-level and repo-independent: the create
	// wizard calls them BEFORE any repo exists, so they cannot live under
	// /repos/{repo}. The ':' action suffix follows the convention noted above.
	r.Post("/ontologies:validate", handleOntologyValidate())
	r.Get("/ontologies/presets", handleOntologyPresets())
	r.Get("/ontologies/presets/{name}", handleOntologyPresetYAML())
	r.Get("/ontologies/schema", handleOntologySchema())

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

			r.Get("/", handleHALRepo(b, p.branchRootReader, s.AgentBranch, s.EmbeddingsEnabled))
			r.Patch("/", handleHALRepoPatch(b))

			// Inside the middleware group, unlike Archive: {repo} must exist
			// for a rename, so the middleware's 404 is the right answer, and
			// every error this handler raises is about the NEW name, which the
			// middleware never sees.
			r.Post("/rename", handleHALRepoRename(b, s.Manager))

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
				r.Get("/motifs", handleHALMotifs(b, p.motifs))
				r.Get("/motifs/{key}", handleHALMotifCluster(b, p.motifs, p.factsCollection))
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

	// Unscoped MCP mount: the session picks its repo/lens through knomit_bind.
	r.Group(func(r chi.Router) {
		r.Use(SessionBindingMiddleware(s.Manager))
		r.HandleFunc("/mcp", mcpDispatch.ServeHTTP)
		r.HandleFunc("/mcp/*", mcpDispatch.ServeHTTP)
	})

	// Lens collection: no {lens} segment, so unwrapped.
	r.Get("/lenses", handleHALLenses(b, s.Manager))
	r.Post("/lenses", handleHALLensesCreate(b, s.Manager))

	r.Route("/lenses/{lens}", func(r chi.Router) {
		// The lens CRUD quartet — including rename — resolves through the
		// registry directly and reports its own errors, so it stays outside
		// the binding group below.
		r.Get("/", handleHALLens(b, s.Manager, p.branchRootReader, s.AgentBranch, s.EmbeddingsEnabled))
		r.Patch("/", handleHALLensPatch(b, s.Manager))
		r.Delete("/", handleHALLensDelete(s.Manager))
		r.Post("/rename", handleHALLensRename(b, s.Manager))

		r.Group(func(r chi.Router) {
			r.Use(LensMiddleware(s.Manager))
			r.Get("/facts", handleHALLensFacts(p.factsCollection, p.motifs, s.Embedder))
			r.Get("/facts/*", handleHALLensFact(b, p.factReader, p.factSub))
			r.Get("/search", handleHALLensSearch(p.search, s.Embedder, p.motifs))
			r.Get("/completions", handleHALLensCompletions(p.completions))
			r.Get("/motifs", handleHALLensMotifs(b, p.motifs))
			r.Get("/motifs/{key}", handleHALLensMotifCluster(b, p.motifs, p.factsCollection))
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
