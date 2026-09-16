package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/client/sessions"
	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// sessionBoundE2E stands up the REAL router with the REAL MCP server over a
// manager holding one writable repo and one subscription, so the whole path —
// SessionBindingMiddleware, knomit_bind, the stored pin, and the tool handlers
// — runs end to end over HTTP.
func sessionBoundE2E(t *testing.T) (http.Handler, *sessions.Store) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	newE2EMount(t, m, "alpha", false)
	newE2EMount(t, m, "followed", true)

	store := newClientSessionsStore(t)
	m.SetClientSessions(store)

	s := &Server{Manager: m, ClientSessions: store, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return s.NewAPIRouter(), store
}

func newE2EMount(t *testing.T, m *repos.Manager, name string, subscribed bool) {
	t.Helper()
	branch := "agent/test"
	if subscribed {
		branch = "main"
	}
	svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	uid := "uid-" + name
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: uid, Name: name, State: repos.StateActive, Profile: "code", CreatedAt: 1,
	}))
	cfg := repos.TestInstanceConfig{
		Name: name, UID: uid, Svc: svc,
		Ontology: fact.CodeOntology(), OntologyRoot: "kb",
	}
	if subscribed {
		cfg.Subscribed = true
		cfg.ReadBranch = "main"
	} else {
		cfg.AgentBranch = "agent/test"
	}
	m.Set(name, repos.NewTestInstanceWithDeps(cfg))
}

// rpc posts one JSON-RPC message to the unscoped mount and returns the decoded
// response, threading the session id the way a real client does.
func rpc(t *testing.T, h http.Handler, sid, body string) (map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	payload := rec.Body.String()
	// A streamable-HTTP server may answer as SSE; take the first data: line.
	if strings.HasPrefix(strings.TrimSpace(payload), "event:") || strings.Contains(payload, "\ndata: ") {
		sc := bufio.NewScanner(strings.NewReader(payload))
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				payload = rest
				break
			}
		}
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &out), "payload: %s", payload)

	got := rec.Header().Get("Mcp-Session-Id")
	if got == "" {
		got = sid
	}
	return out, got
}

// callTool returns the tool result's text and whether it was an error result.
func callTool(t *testing.T, h http.Handler, sid, name, args string) (string, bool) {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args)
	resp, _ := rpc(t, h, sid, body)

	result, ok := resp["result"].(map[string]any)
	require.True(t, ok, "no result in %v", resp)
	isErr, _ := result["isError"].(bool)

	var sb strings.Builder
	content, _ := result["content"].([]any)
	for _, c := range content {
		if cm, ok := c.(map[string]any); ok {
			if s, ok := cm["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String(), isErr
}

// The whole session-bound story over HTTP: nothing works until knomit_bind,
// binding a writable repo opens the tools, and re-binding to a subscription
// keeps reads working while writes are refused.
func TestSessionBoundMCP_EndToEnd(t *testing.T) {
	h, sessStore := sessionBoundE2E(t)

	// initialize: no session id yet, so this passes through unbound.
	initResp, sid := rpc(t, h, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1.0"}}}`)
	require.NotEmpty(t, sid, "server must mint a session id")
	instr, _ := initResp["result"].(map[string]any)["instructions"].(string)
	require.Contains(t, instr, "knomit_bind", "unscoped initialize must say to bind first")

	// The row for the id the server just MINTED must carry no pin: mcp-go
	// ignores the inbound Mcp-Session-Id on initialize, so a pin here would be
	// some earlier session's.
	require.Equal(t, "", bindingOf(t, sessStore, sid),
		"initialize must not stamp a binding on the newly minted session")

	// Unbound: every tool fails closed, naming the tool to call.
	text, isErr := callTool(t, h, sid, "knomit_repos", `{}`)
	require.True(t, isErr, "knomit_repos must fail while unbound: %s", text)
	require.Contains(t, text, "knomit_bind")

	// Bind the writable repo.
	text, isErr = callTool(t, h, sid, "knomit_bind", `{"repo":"alpha"}`)
	require.False(t, isErr, text)
	require.Contains(t, text, `"role": "read+write"`)

	// The client_sessions pin lags by exactly one request, and that is the
	// recording model, not a bug: recordClientSession stamps the pin from the
	// context the middleware built BEFORE this call, and at that moment the
	// session was still unbound. It appears on the next request.
	require.Equal(t, "", bindingOf(t, sessStore, sid),
		"the bind call itself was still an unbound request when it arrived")

	// Now knomit_repos answers, and the binding survived in the store.
	text, isErr = callTool(t, h, sid, "knomit_repos", `{}`)
	require.False(t, isErr, text)
	require.Equal(t, "repo:uid-alpha", bindingOf(t, sessStore, sid),
		"the pin lands on the first request AFTER the bind")
	require.Contains(t, text, `"name": "alpha"`)
	require.Contains(t, text, `"role": "read+write"`)

	// Switch to the subscription.
	text, isErr = callTool(t, h, sid, "knomit_bind", `{"repo":"followed"}`)
	require.False(t, isErr, text)
	require.Contains(t, text, `"mode": "subscribe"`)

	// A write is refused — by the binding's own WriteOK, not by new logic.
	text, isErr = callTool(t, h, sid, "knomit_learn",
		`{"topic":"architecture","category":"x/y","title":"t","body":"b"}`)
	require.True(t, isErr, "a subscription must refuse writes: %s", text)
	require.Contains(t, text, "read-only view")

	// ...but reads still work on the very same binding.
	text, isErr = callTool(t, h, sid, "knomit_query", `{"text":"anything"}`)
	require.False(t, isErr, "a subscription must still serve reads: %s", text)
}

// bindingOf returns the binding column of one client_sessions row, or "" when
// the row does not exist yet.
func bindingOf(t *testing.T, st *sessions.Store, sid string) string {
	t.Helper()
	rows, err := st.List(context.Background(), sessions.Filter{Now: time.Now(), IncludeHidden: true})
	require.NoError(t, err)
	for _, r := range rows {
		if r.ID == sid {
			return r.Binding
		}
	}
	return ""
}
