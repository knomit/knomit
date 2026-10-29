package web

import (
	"context"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"knomit/internal/edge/web/api/jobs"
	"knomit/internal/edge/web/api/kb"
	"knomit/internal/edge/web/api/manage"
	"knomit/internal/edge/web/api/origins"
	"knomit/internal/edge/web/hal"
	"knomit/internal/foundation/auth"
)

// NewAPIRouter constructs the HAL chi router for this Server. The router
// is rooted at "/" — the caller mounts it under httpx.APIBase via chi.Mount.
// Everything below this router assumes httpx.APIBase as the Server-level prefix.
//
// Middleware order:
//  1. Recoverer — catch panics, return 500 problem+json
//  2. compressor() — the shared allowlist from compress.go. It MUST be the
//     shared one: chi's built-in list has no application/hal+json, so the
//     bare middleware.Compress(5) that stood here compressed nothing at all.
//  3. NotFound / MethodNotAllowed handlers — return problem+json
//
// Per-route middleware (httpx.BranchMiddleware, RepoMiddleware) is attached at
// the route-group level where branches/repos appear in the path, not at
// the router root.
func (s *Server) NewAPIRouter() chi.Router {
	r := s.apiRouter(s.authEdge(), s.grants())
	// The operator's approval endpoints live on THIS router only, never on
	// the OAuth listener's, and only when [oauth] is configured.
	if s.OAuthIssuer != nil {
		s.mountOAuthPending(r)
	}
	return r
}

// apiRouter is the API and MCP tree behind ONE edge middleware — the thing
// that turns a transport into a principal — and the Grants its write gate
// consults. NewAPIRouter passes AuthMiddleware (socket, certificate,
// loopback policy); OAuthHandler passes the bearer middleware and a Grants
// that knows no anonymous principal. The OAuth router must never run
// AuthMiddleware: for a loopback peer — a same-host reverse proxy — it would
// replace the token principal with anonymous, which holds loopback_default.
func (s *Server) apiRouter(edge func(http.Handler) http.Handler, g auth.Grants) chi.Router {
	r := chi.NewRouter()
	// Correlation id for slow-request warnings. chi echoes an inbound
	// X-Request-Id verbatim and only generates one when the header is absent, so
	// req_id is trustworthy exactly as far as the client is: useful for stitching
	// a proxy's id to ours, forgeable by anyone who wants two requests to look
	// like one. It labels log lines only — never an authorization decision.
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer) // produces the 500 response
	r.Use(reportPanic)          // captures a crash bundle, re-panics
	// Every request gets a principal here, before anything can act on it.
	//
	// BELOW Recoverer and reportPanic, so a panic in here becomes a 500
	// problem document and a crash bundle rather than a dropped connection —
	// and ABOVE readOnlyGate and writeGate, which cannot decide anything
	// without the principal this attaches. Both halves of that sandwich are
	// load-bearing; moving it outside either one is a regression.
	//
	// The outer router in Handler() runs this too (also below its Recoverer),
	// so /git inherits a principal as well; running twice is idempotent — the
	// same connection yields the same answer — and it keeps a
	// directly-constructed API router authenticated.
	r.Use(edge)
	r.Use(metricsMiddleware(nil, s.SlowRequestMS)) // nil → metrics.Default
	r.Use(compressor())
	if s.ReadOnly {
		r.Use(readOnlyGate)
	}
	// After readOnlyGate, so the instance-level message wins over the
	// caller-level one when both would refuse.
	r.Use(writeGate(g, s.authDisabled))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusNotFound, "Not Found", "no resource at "+req.URL.Path, req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		hal.WriteProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed",
			"method "+req.Method+" not allowed on "+req.URL.Path, req.URL.Path)
	})

	b := hal.URLBuilder{Base: httpx.APIBase}

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
		// The PATH is what the client addressed; `branch` is what was actually
		// served, and it is logged whenever the URL does not ALREADY NAME the
		// served branch — which is exactly when the path alone misleads. A
		// session inside an experiment writes to exp/<name> while every log
		// line still reads …/branches/agent:…/mcp, so someone reading the log
		// to find out where a fact went is told the wrong answer with no hint
		// that it is wrong. A lens mount has no branch in its URL at all, so
		// the field is always present there.
		//
		// The path is never rewritten. It stays the address the caller used,
		// because that is what they can correlate with their own config.
		done := log.Info().
			Str("method", req.Method).
			Str("path", req.URL.Path).
			Int("status", mw.status).
			Str("response_content_type", mw.Header().Get("Content-Type")).
			Dur("elapsed", time.Since(start))
		if served := servedBranch(req.Context()); served != "" {
			done = done.Str("branch", served)
		}
		done.Msg("mcp: request done")
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
	r.Get("/logs/events", handleLogEvents(s.Logs, s.ReadOnly))

	// The route skeleton. This router owns every subtree and the middleware
	// that wraps it, because the KB routes, the MCP mounts and the lifecycle
	// routes (api/manage) share them: it builds the subrouters, puts each
	// one's middleware on BEFORE any route is registered on it, and hands
	// them to manage.Mount, kb.Mount and jobs.Mount.
	//
	// /repos/{repo}: no generic route mutates the FLEET repository
	// (httpx.FleetRepoRefused), Archive included: it changes only through
	// /fleet. repo is the group inside it that resolves {repo} exactly once,
	// in RepoMiddleware; handlers read the instance with repos.RepoFromContext.
	repoOuter := r.Route("/repos/{repo}", func(chi.Router) {})
	repoOuter.Use(httpx.FleetRepoRefused(s.Manager))
	repo := repoOuter.Group(nil)
	repo.Use(RepoMiddleware(s.Manager))
	branch := repo.Route("/branches/{branch}", func(chi.Router) {})
	branch.Use(httpx.BranchMiddleware)
	lens := r.Route("/lenses/{lens}", func(chi.Router) {})

	operator := requireOperator(g, s.authDisabled)
	manage.Mount(r, repoOuter, repo, branch, lens,
		s.Manager, s.AgentBranch, s.EmbeddingsEnabled, s.ExperimentExpiryDays,
		operator)

	// The connect-a-remote wizard (api/origins). Inside the
	// RepoMiddleware group: its handlers read the repo from context. Every
	// route, reads included, is the operator's, like PUT /origin: a session
	// clones the remote it names and previews its content, and its commit
	// attaches or swaps in that remote.
	repo.With(operator).Mount("/origin-sessions", s.originSessionsRouter())

	// The lens subtree INSIDE LensMiddleware: the KB's federated reads and
	// the lens MCP mount below. The CRUD (api/manage) sits outside it, on
	// lens itself, because it resolves {lens} through the registry.
	lensBound := lens.Group(nil)
	lensBound.Use(LensMiddleware(s.Manager))

	// The KB surface (api/kb): a branch's facts, topics, commits, search and
	// the other reads, and a lens's federated reads.
	kb.Mount(branch, lensBound, s.OntologyRoot, s.Embedder, s.Manager)

	// Synthesis-run and index-rebuild jobs (api/jobs).
	jobs.Mount(branch, s.Manager, s.LLMAdapter)

	// ONLY the MCP mount is marked as one where a binding handle may
	// choose the branch. The REST routes above name their branch in the
	// URL and must keep meaning exactly that: a GET of
	// /branches/agent:x/facts is a question about agent:x whoever asks
	// it.
	branch.Group(func(r chi.Router) {
		r.Use(ScopedMountMiddleware(s.Manager))
		r.HandleFunc("/mcp", mcpDispatch.ServeHTTP)
		r.HandleFunc("/mcp/*", mcpDispatch.ServeHTTP)
	})

	// Unscoped MCP mount: the session picks its repo/lens through knomit_bind.
	r.Group(func(r chi.Router) {
		r.Use(SessionBindingMiddleware(s.Manager))
		// httpx.IsUnscopedMCP is the same set of paths, for the bearer
		// edge's reach: keep both on these constants.
		r.HandleFunc(httpx.UnscopedMCPPath, mcpDispatch.ServeHTTP)
		r.HandleFunc(httpx.UnscopedMCPPattern, mcpDispatch.ServeHTTP)
	})

	// As on the repo subtree: the handle marker applies to the lens MCP
	// mount only. The KB's lens reads (kb.Mount above) federate at each
	// member's own pinned branch and must keep doing so.
	lensBound.Group(func(r chi.Router) {
		r.Use(ScopedMountMiddleware(s.Manager))
		r.HandleFunc("/mcp", mcpDispatch.ServeHTTP)
		r.HandleFunc("/mcp/*", mcpDispatch.ServeHTTP)
	})

	// The lens twin of /repos/{repo}/branches/exp:<name>/mcp. A lens pins
	// a branch per member, so it has no single branch segment to carry an
	// experiment; it gets its own, and only the WRITE member moves.
	//
	// MCP only, deliberately. The HAL reads under /lenses/{lens} federate
	// across members at their own pinned branches, and an experiment
	// belongs to exactly one of them — there is no coherent whole-lens
	// read "inside" an experiment to serve here.
	lens.Route("/experiments/{name}", func(r chi.Router) {
		r.Use(LensExperimentMiddleware(s.Manager))
		r.HandleFunc("/mcp", mcpDispatch.ServeHTTP)
		r.HandleFunc("/mcp/*", mcpDispatch.ServeHTTP)
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

// originSessionsRouter returns the origin-session subtree, built by
// origins.Mount ONCE per Server. Mount creates the session manager its routes
// use, and apiRouter runs once for Handler and again for OAuthHandler, so a
// Mount per apiRouter would give each listener its own manager: a session
// created on one listener would be unknown on the other, and the
// one-session-per-repo rule would hold per listener only. chi's Route is Mount
// of a fresh router, so mounting this shared one leaves the route tree and
// its middleware exactly as an inline r.Route would.
//
// One coupling the sharing adds: when a parent mounts a subrouter, chi
// (v5 Mux.Mount) copies the parent's NotFound and MethodNotAllowed handlers
// into it unless the subrouter already has its own. The copy comes from the
// first parent to mount this router (Handler's apiRouter, or OAuthHandler's if
// that is built first); the second mount finds handlers already set and
// leaves them. That is harmless only while every apiRouter installs the SAME
// NotFound and MethodNotAllowed (the problem+json ones at the top of
// apiRouter): a per-listener handler would answer the second listener's
// unmatched origin-session paths with the first listener's.
//
// The manager lives as long as s.Lifetime (nil: forever) and keeps its
// session folders in s.OriginSessionsDir (see those fields).
func (s *Server) originSessionsRouter() chi.Router {
	s.originSessionsOnce.Do(func() {
		ctx := s.Lifetime
		// With no Lifetime the manager never releases, so it is not counted:
		// a Wait on originSessionsReleased would block forever.
		var released *sync.WaitGroup
		if ctx == nil {
			ctx = context.Background()
		} else {
			released = &s.originSessionsReleased
		}
		r := chi.NewRouter()
		origins.Mount(ctx, r, s.Manager, s.AgentBranch, s.OriginSessionsDir, released)
		s.originSessions = r
	})
	return s.originSessions
}
