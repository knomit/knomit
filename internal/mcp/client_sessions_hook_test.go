package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
	storemigrate "knomit/internal/store/migrate"
)

func newSessionsStore(t *testing.T) *sessions.Store {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := storemigrate.Control(db); err != nil {
		t.Fatal(err)
	}
	return sessions.New(db, sessions.Policy{DeadAfter: time.Hour, HiddenAfter: 3 * time.Hour})
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.0"}}}`

func postInitialize(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(initializeBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// initialize is the ONLY place the host's declared client name/version is
// available, so the hook is what puts it on the row.
func TestAfterInitialize_RecordsClientInfo(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	store := newSessionsStore(t)
	m.SetClientSessions(store)

	h := mcpserver.NewStreamableHTTPServer(NewServer("kb", m, false))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := postInitialize(t, srv.URL)
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session id minted")
	}

	rows, _, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != sid || rows[0].ClientName != "claude-code" ||
		rows[0].ClientVersion != "2.1.0" || !rows[0].Initialized {
		t.Fatalf("%+v", rows)
	}
}

// A nil manager or an unstarted one (no store) must not fail the initialize:
// recording is never on the critical path.
func TestAfterInitialize_NilManagerAndNilStoreAreSafe(t *testing.T) {
	for _, m := range []*repos.Manager{nil, repos.New(context.Background(), repos.Deps{})} {
		h := mcpserver.NewStreamableHTTPServer(NewServer("kb", m, false))
		srv := httptest.NewServer(h)
		resp := postInitialize(t, srv.URL)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		resp.Body.Close()
		srv.Close()
	}
}

// The repo-scoped route puts no explicit Binding in the context, only a
// RepoInstance — so a hook reading BindingFromContextOpt alone records an
// empty binding for the single-repo path, which is the common one.
func TestAfterInitialize_RecordsBindingOnTheRepoRoute(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "u-alpha", AgentBranch: "agent/test",
	}))
	store := newSessionsStore(t)
	m.SetClientSessions(store)

	srv := httptest.NewServer(repoScopedMCP(t, m, "alpha", "agent/test"))
	defer srv.Close()

	resp := postInitialize(t, srv.URL)
	resp.Body.Close()

	rows, _, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Binding != "repo:u-alpha" {
		t.Fatalf("binding not recorded on the repo route: %+v", rows)
	}
}

// repoScopedMCP wraps the MCP handler the way the repo-scoped web route does:
// a RepoInstance and a branch in the context, and NO explicit Binding.
func repoScopedMCP(t *testing.T, m *repos.Manager, repo, branch string) http.Handler {
	t.Helper()
	h := mcpserver.NewStreamableHTTPServer(NewServer("kb", m, false))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := repos.WithRepoInstance(r.Context(), m.Get(repo))
		ctx = repos.WithBranch(ctx, branch)
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// mcp-go does not put the *http.Request in the hook's context, so the wiring
// that does (WithHTTPContextFunc, in internal/web) is what lets a direct HTTP
// client's identity be derived from the request that declared its clientInfo.
// Without it the row is written with ” for both and every such client
// collides.
func TestAfterInitialize_RecordsObservedConnectionDetails(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	store := newSessionsStore(t)
	m.SetClientSessions(store)

	h := mcpserver.NewStreamableHTTPServer(NewServer("kb", m, false),
		mcpserver.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			return sessions.WithHTTPInfo(ctx, r.RemoteAddr, r.Header.Get("User-Agent"))
		}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewBufferString(initializeBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "mcp-inspector/0.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rows, _, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	// No Touch has run — these can only have come from the request itself.
	if rows[0].RemoteAddr == "" || rows[0].UserAgent != "mcp-inspector/0.9" {
		t.Fatalf("observed connection details not recorded: %+v", rows[0])
	}
	if rows[0].InstanceID != sessions.DeriveInstanceID(rows[0].RemoteAddr, "mcp-inspector/0.9", "claude-code", "2.1.0") {
		t.Fatalf("instance id not derived from the request: %+v", rows[0])
	}
	if rows[0].RequestCount != 0 {
		t.Fatalf("initialize carries no session id, so it must not count as a request: %+v", rows[0])
	}
}
