package web

import (
	"context"
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
