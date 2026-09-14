package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
	storemigrate "knomit/internal/store/migrate"
)

func newClientSessionsStore(t *testing.T) *sessions.Store {
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
	return sessions.New(db, sessions.Policy{DeadAfter: time.Hour, HiddenAfter: 3 * time.Hour, Retention: 168 * time.Hour})
}

// The recorded binding is a PinID, which fails closed on a uid-less
// instance — so these fixtures carry a uid, unlike newTestManagerWithRepos.
func newTestManagerWithUIDRepo(t *testing.T, name, uid string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: name, UID: uid, AgentBranch: "agent/test",
	}))
	return m
}

// stubMCP stands in for the real streamable HTTP server: it answers with the
// status the test asks for, so the dispatch's recording rules can be pinned
// without an MCP handshake.
func stubMCP(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
}

func TestMCPDispatch_RecordsSessions(t *testing.T) {
	store := newClientSessionsStore(t)
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), ClientSessions: store, mcpHandler: stubMCP(200)}
	r := s.NewAPIRouter()
	hdr := sessions.BridgeInfo{InstanceID: "inst", Transport: "stdio", PID: 7, Host: "h", Cwd: "/w", Branch: "agent/test", Version: "1"}

	post := func(sid, client, method string) int {
		req := httptest.NewRequest(method, "/repos/alpha/branches/agent:test/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.RemoteAddr = "192.0.2.9:54321"
		req.Header.Set("User-Agent", "knomit-bridge/1")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		if client != "" {
			req.Header.Set(sessions.ClientHeader, client)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	post("", hdr.Encode(), http.MethodPost)             // initialize-shaped: no session id ⇒ nothing recorded
	post("sid-1", hdr.Encode(), http.MethodPost)        // recorded, stdio
	post("sid-1", hdr.Encode(), http.MethodPost)        // bumped
	post("sid-2", "", http.MethodPost)                  // direct http caller
	post("sid-2", "garbage-no-equals", http.MethodPost) // unparseable ⇒ still http

	rows, err := store.List(context.Background(), sessions.Filter{Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]sessions.Session{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d %+v", len(rows), rows)
	}
	if r1 := byID["sid-1"]; r1.RequestCount != 2 || r1.Transport != "stdio" || r1.InstanceID != "inst" ||
		r1.RemoteAddr != "192.0.2.9" || r1.Binding != "repo:u-alpha" {
		t.Fatalf("sid-1: %+v", r1)
	}
	if r2 := byID["sid-2"]; r2.RequestCount != 2 || r2.Transport != "http" || r2.InstanceID == "" {
		t.Fatalf("sid-2: %+v", r2)
	}

	// DELETE ends the session.
	post("sid-1", hdr.Encode(), http.MethodDelete)
	rows, _ = store.List(context.Background(), sessions.Filter{Now: time.Now()})
	for _, row := range rows {
		if row.ID == "sid-1" && (row.Ended == nil || row.State != sessions.StateDead) {
			t.Fatalf("not ended: %+v", row)
		}
	}
}

func TestMCPDispatch_404CreatesNothing_NilStoreSafe(t *testing.T) {
	store := newClientSessionsStore(t)
	s := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), ClientSessions: store, mcpHandler: stubMCP(404)}
	r := s.NewAPIRouter()
	req := httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/mcp", strings.NewReader(`{}`))
	req.Header.Set("Mcp-Session-Id", "stale")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	rows, _ := store.List(context.Background(), sessions.Filter{Now: time.Now(), IncludeHidden: true})
	if len(rows) != 0 {
		t.Fatalf("404 must not create a row: %+v", rows)
	}

	s2 := &Server{Manager: newTestManagerWithUIDRepo(t, "alpha", "u-alpha"), mcpHandler: stubMCP(200)} // no store
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/repos/alpha/branches/agent:test/mcp", strings.NewReader(`{}`))
	req.Header.Set("Mcp-Session-Id", "x")
	s2.NewAPIRouter().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("nil store must be transparent: %d", rec.Code)
	}
}
