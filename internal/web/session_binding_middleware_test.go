package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
)

// probeCtx runs one GET through SessionBindingMiddleware and hands the
// resulting context back, with the status the next handler produced.
func probeCtx(t *testing.T, m *repos.Manager, sid string) (context.Context, int) {
	t.Helper()
	var seen context.Context
	r := chi.NewRouter()
	r.With(SessionBindingMiddleware(m)).Get("/mcp", func(w http.ResponseWriter, req *http.Request) {
		seen = req.Context()
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.NotNil(t, seen, "the next handler must always run")
	return seen, rec.Code
}

// The middleware marks the context session-scoped and installs a pin recorder,
// and does nothing else. Those two are what knomit_bind and the tool gate read.
func TestSessionBindingMiddleware_MarksScopeAndInstallsRecorder(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	m.SetClientSessions(newClientSessionsStore(t))

	ctx, code := probeCtx(t, m, "")
	require.Equal(t, http.StatusOK, code)
	require.True(t, repos.SessionScoped(ctx))
	rec, ok := repos.PinRecorderFromContext(ctx)
	require.True(t, ok, "the gate needs somewhere to report the resolved pin")
	require.Equal(t, repos.ResolvedBinding{}, rec.Resolved(), "nothing has been resolved yet")

	_, ok = repos.BindingFromContextOpt(ctx)
	require.False(t, ok)
	_, hasErr := repos.BindingErrorFromContext(ctx)
	require.False(t, hasErr)
}

// THE REGRESSION GUARD FOR THE INCIDENT, at the middleware.
//
// Binding used to be looked up here, from Mcp-Session-Id. Because a client may
// share one connection — and so one session id — across several logical jobs,
// that made every request on the connection resolve to whatever the connection
// had bound LAST, and two Cowork jobs' writes crossed over. So the guard is not
// "the lookup returns the right thing"; it is that THERE IS NO LOOKUP. No
// session id, however well-known, may put a Binding in the context.
//
// If a future change reintroduces any session-keyed resolution here, this fails.
func TestSessionBindingMiddleware_ResolvesNothingFromTheSessionID(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	// A live handle exists, and its value is even used as the session id — the
	// most favourable case any session-keyed lookup could possibly have.
	require.NoError(t, store.MintBindingHandle(context.Background(), "sid-1", "repo:u-alpha", "", time.Now()))

	for _, sid := range []string{"", "sid-1", "sid-unknown"} {
		ctx, code := probeCtx(t, m, sid)
		require.Equal(t, http.StatusOK, code, "sid=%q", sid)
		require.True(t, repos.SessionScoped(ctx), "sid=%q", sid)
		_, ok := repos.BindingFromContextOpt(ctx)
		require.False(t, ok, "sid=%q must not resolve a Binding", sid)
		_, ok = repos.RepoFromContextOpt(ctx)
		require.False(t, ok, "sid=%q must not resolve a RepoInstance", sid)
		_, hasErr := repos.BindingErrorFromContext(ctx)
		require.False(t, hasErr, "sid=%q", sid)
	}
}

// A manager with no sessions store is still fine here: the middleware never
// touches one.
func TestSessionBindingMiddleware_NilStore(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")

	ctx, code := probeCtx(t, m, "sid-3")
	require.Equal(t, http.StatusOK, code)
	require.True(t, repos.SessionScoped(ctx))
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok)
}

// The unscoped mount is wired into the real API router, and the pin the tool
// gate resolved lands on the client_sessions row like any other MCP request —
// travelling back out through the recorder, since on this mount nothing knows
// the binding until the handler has run.
//
// It stays OBSERVATIONAL: the column records what the session was seen doing
// and gates nothing. Routing came from the handle.
func TestUnscopedMCPMount_RecordsBindingPin(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)

	// Stand in for the tool gate: record a pin from inside the handler, which
	// is where it becomes known.
	s := &Server{Manager: m, ClientSessions: store,
		mcpHandler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			rec, ok := repos.PinRecorderFromContext(req.Context())
			require.True(t, ok)
			rec.Record(repos.ResolvedBinding{Handle: "h-1", Pin: "repo:u-alpha"})
			w.WriteHeader(http.StatusOK)
		})}
	router := s.NewAPIRouter()

	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Mcp-Session-Id", "sid-4")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	rows, _, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
	require.NoError(t, err)
	var found bool
	for _, row := range rows {
		if row.ID == "sid-4" {
			found = true
			require.Equal(t, "repo:u-alpha", row.Binding)
		}
	}
	require.True(t, found, "the unscoped mount must record the session like any other")
}

// A request whose handler recorded nothing leaves the column empty rather than
// inheriting some other request's pin.
func TestUnscopedMCPMount_NoPinRecordedLeavesColumnEmpty(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)

	s := &Server{Manager: m, ClientSessions: store, mcpHandler: stubMCP(200)}
	router := s.NewAPIRouter()

	req := fromLoopback(httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Mcp-Session-Id", "sid-5")
	router.ServeHTTP(httptest.NewRecorder(), req)

	rows, _, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
	require.NoError(t, err)
	for _, row := range rows {
		if row.ID == "sid-5" {
			require.Equal(t, "", row.Binding)
			return
		}
	}
	t.Fatal("row not recorded")
}

// The middleware must not read the request body. It used to peek it to detect
// an initialize — machinery that existed only to keep the session lookup off
// the one request whose header names a different session — and every byte of
// that went with the lookup. A body arrives at the handler untouched, at any
// size, whatever it contains.
func TestSessionBindingMiddleware_BodyUntouched(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	m.SetClientSessions(newClientSessionsStore(t))

	for name, body := range map[string]string{
		"initialize": `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		"tools/call": `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knomit_repos"}}`,
		"not json":   "not json at all",
		"empty":      "",
		"huge": `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knomit_learn","arguments":{"body":"` +
			strings.Repeat("x", 32*1024) + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var readBack string
			r := chi.NewRouter()
			r.With(SessionBindingMiddleware(m)).Post("/mcp", func(w http.ResponseWriter, req *http.Request) {
				b, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				readBack = string(b)
				w.WriteHeader(http.StatusOK)
			})
			req := fromLoopback(httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))
			req.Header.Set("Mcp-Session-Id", "sid-old")
			r.ServeHTTP(httptest.NewRecorder(), req)
			require.Equal(t, body, readBack)
		})
	}
}

// TestSessionBindingMiddleware_ResolvesNoExperimentFromTheSessionID extends
// the sibling above to the thing PR 2 added.
//
// The active experiment is keyed on the BINDING HANDLE, and the reason is the
// 2026-09-17 incident: two logical jobs can share one connection and
// therefore one Mcp-Session-Id, so anything keyed on that id serves whichever
// job acted last. This asserts the negative directly, under the most
// favourable input a session-keyed lookup could possibly get — a LIVE handle
// that is genuinely inside an experiment, whose own value is then used AS the
// session id.
//
// It lives at the middleware rather than end-to-end on purpose: mcp-go
// validates session ids, so the transport refuses an invented one before any
// lookup could happen, and a test that went through it would pass for the
// wrong reason.
func TestSessionBindingMiddleware_ResolvesNoExperimentFromTheSessionID(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)

	ctx := context.Background()
	require.NoError(t, store.MintBindingHandle(ctx, "sid-1", "repo:u-alpha", "", time.Now()))
	require.NoError(t, store.SetHandleExperiment(ctx, "sid-1", "in-an-experiment", time.Now()))
	got, err := store.HandleExperiment(ctx, "sid-1")
	require.NoError(t, err)
	require.Equal(t, "in-an-experiment", got,
		"precondition: the handle really is inside an experiment, so a session-keyed lookup would have something to find")

	for _, sid := range []string{"", "sid-1", "sid-unknown"} {
		reqCtx, code := probeCtx(t, m, sid)
		require.Equal(t, http.StatusOK, code, "sid=%q", sid)
		// No binding at all means no experiment either — the experiment
		// travels on the binding, and the binding comes from the handle
		// ARGUMENT, which the middleware never sees.
		_, ok := repos.BindingFromContextOpt(reqCtx)
		require.False(t, ok, "sid=%q must not resolve a Binding, and so cannot carry an experiment", sid)
		require.Empty(t, repos.LapsedExperimentFromContext(reqCtx),
			"sid=%q must not even decide an experiment LAPSED — nothing experiment-shaped is read from the session id", sid)
	}
}
