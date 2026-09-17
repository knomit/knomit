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

// No session id at all (the initialize request): session-scoped, unbound, and
// the handler still runs — initialize has no id yet by design.
func TestSessionBindingMiddleware_NoSessionID(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	m.SetClientSessions(newClientSessionsStore(t))

	ctx, code := probeCtx(t, m, "")
	require.Equal(t, http.StatusOK, code)
	require.True(t, repos.SessionScoped(ctx))
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok)
	_, hasErr := repos.BindingErrorFromContext(ctx)
	require.False(t, hasErr)
}

// A session id with no stored row is simply unbound — not an error.
func TestSessionBindingMiddleware_UnboundSession(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	m.SetClientSessions(newClientSessionsStore(t))

	ctx, code := probeCtx(t, m, "sid-unknown")
	require.Equal(t, http.StatusOK, code)
	require.True(t, repos.SessionScoped(ctx))
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok)
	_, hasErr := repos.BindingErrorFromContext(ctx)
	require.False(t, hasErr)
}

// A stored pin that resolves puts BOTH the Binding and the write RepoInstance
// in the context — the same shape LensMiddleware produces.
func TestSessionBindingMiddleware_BoundRepo(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-1", "repo:u-alpha", time.Now()))

	ctx, code := probeCtx(t, m, "sid-1")
	require.Equal(t, http.StatusOK, code)

	b, ok := repos.BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.Equal(t, "repo:u-alpha", b.PinID())
	ri, ok := repos.RepoFromContextOpt(ctx)
	require.True(t, ok)
	require.Equal(t, "alpha", ri.Name())
}

// A stored pin that no longer resolves is a context ERROR, not a 4xx: the
// caller speaks JSON-RPC and a status code would break the framing and tell
// the agent nothing it could act on.
func TestSessionBindingMiddleware_UnresolvablePin(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-2", "repo:gone", time.Now()))

	ctx, code := probeCtx(t, m, "sid-2")
	require.Equal(t, http.StatusOK, code, "must NOT be an HTTP failure")

	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok)
	err, hasErr := repos.BindingErrorFromContext(ctx)
	require.True(t, hasErr)
	require.Contains(t, err.Error(), "knomit_bind")
}

// A manager with no sessions store is treated as unbound, never a 500.
func TestSessionBindingMiddleware_NilStore(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")

	ctx, code := probeCtx(t, m, "sid-3")
	require.Equal(t, http.StatusOK, code)
	require.True(t, repos.SessionScoped(ctx))
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok)
}

// The unscoped mount is wired into the real API router, and a bound request
// records that pin on the client_sessions row like any other MCP request.
func TestUnscopedMCPMount_RecordsBindingPin(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-4", "repo:u-alpha", time.Now()))

	s := &Server{Manager: m, ClientSessions: store, mcpHandler: stubMCP(200)}
	router := s.NewAPIRouter()

	req := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Mcp-Session-Id", "sid-4")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	rows, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
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

// probePost runs one POST with a body through the middleware, returning the
// resulting context and the body bytes the NEXT handler managed to read.
func probePost(t *testing.T, m *repos.Manager, sid, body string) (context.Context, string) {
	t.Helper()
	var seen context.Context
	var readBack string
	r := chi.NewRouter()
	r.With(SessionBindingMiddleware(m)).Post("/mcp", func(w http.ResponseWriter, req *http.Request) {
		seen = req.Context()
		b, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		readBack = string(b)
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
	require.NotNil(t, seen)
	return seen, readBack
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
const callBody = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knomit_repos"}}`

// An initialize carrying a STALE session id must not resolve that session's
// binding: mcp-go mints a new id for it, so the header names a different
// session than the response will.
func TestSessionBindingMiddleware_InitializeIgnoresStaleSessionID(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-old", "repo:u-alpha", time.Now()))

	ctx, body := probePost(t, m, "sid-old", initBody)

	require.True(t, repos.SessionScoped(ctx))
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok, "initialize must not carry the previous session's Binding")
	_, ok = repos.RepoFromContextOpt(ctx)
	require.False(t, ok, "nor its RepoInstance — recordClientInfo reads the pin off this")
	_, hasErr := repos.BindingErrorFromContext(ctx)
	require.False(t, hasErr)
	require.Equal(t, initBody, body, "the peek must restore the body for the real handler")
}

// Any other method with the same header resolves exactly as before.
func TestSessionBindingMiddleware_NonInitializeStillResolves(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-old", "repo:u-alpha", time.Now()))

	ctx, body := probePost(t, m, "sid-old", callBody)

	b, ok := repos.BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.Equal(t, "repo:u-alpha", b.PinID())
	require.Equal(t, callBody, body)
}

// A body that is not JSON at all must degrade to "not initialize" and still
// reach the handler intact, rather than eating the request.
func TestSessionBindingMiddleware_UnparseableBodyStillResolves(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-old", "repo:u-alpha", time.Now()))

	ctx, body := probePost(t, m, "sid-old", "not json at all")

	_, ok := repos.BindingFromContextOpt(ctx)
	require.True(t, ok, "an undecodable body is not an initialize")
	require.Equal(t, "not json at all", body)
}

// A body larger than the peek window must reach the handler byte-identical —
// the peeked prefix is restored ahead of the still-streaming remainder — and
// must resolve normally, since an oversized body is never an initialize.
func TestSessionBindingMiddleware_OversizeBodyRoundTripsAndResolves(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-old", "repo:u-alpha", time.Now()))

	big := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"knomit_learn","arguments":{"body":"` +
		strings.Repeat("x", peekLimit*3) + `"}}}`
	require.Greater(t, len(big), peekLimit)

	ctx, body := probePost(t, m, "sid-old", big)

	require.Equal(t, big, body, "the whole body must survive the bounded peek")
	b, ok := repos.BindingFromContextOpt(ctx)
	require.True(t, ok, "an oversized body is not an initialize, so it resolves as normal")
	require.Equal(t, "repo:u-alpha", b.PinID())
}

// An initialize body sitting just under the window is still detected — the
// boundary is the thing most likely to rot if peekLimit changes.
func TestSessionBindingMiddleware_InitializeNearPeekLimit(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-old", "repo:u-alpha", time.Now()))

	pad := strings.Repeat("y", peekLimit-200)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"pad":"` + pad + `"}}`
	require.Less(t, len(body), peekLimit)

	ctx, got := probePost(t, m, "sid-old", body)

	require.Equal(t, body, got)
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok, "still an initialize: no stale binding")
}

// The window is safe because the method is read token-wise: an initialize far
// larger than peekLimit is still recognised, so it cannot fall through to the
// stale-binding path.
func TestSessionBindingMiddleware_OversizeInitializeStillDetected(t *testing.T) {
	m := newTestManagerWithUIDRepo(t, "alpha", "u-alpha")
	store := newClientSessionsStore(t)
	m.SetClientSessions(store)
	require.NoError(t, store.BindSession(context.Background(), "sid-old", "repo:u-alpha", time.Now()))

	// method first, then a capabilities blob many times the window.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{"blob":"` +
		strings.Repeat("z", peekLimit*4) + `"}}}`
	require.Greater(t, len(body), peekLimit)

	ctx, got := probePost(t, m, "sid-old", body)

	require.Equal(t, body, got, "body must survive intact")
	_, ok := repos.BindingFromContextOpt(ctx)
	require.False(t, ok, "an oversized initialize must NOT resolve the previous session's pin")
}

func TestMethodOf(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"plain", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, "initialize"},
		{"method last", `{"jsonrpc":"2.0","params":{"a":[1,2,{"b":3}]},"method":"tools/call"}`, "tools/call"},
		{"truncated after method", `{"method":"initialize","params":{"x":"yyyy`, "initialize"},
		{"truncated before method", `{"params":{"x":"yyyy`, ""},
		{"not an object", `[1,2,3]`, ""},
		{"garbage", `not json`, ""},
		{"empty", ``, ""},
		{"no method key", `{"jsonrpc":"2.0","id":1}`, ""},
	} {
		if got := methodOf([]byte(c.in)); got != c.want {
			t.Errorf("%s: methodOf(%q)=%q want %q", c.name, c.in, got, c.want)
		}
	}
}
