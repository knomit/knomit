package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
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
	return rpcAt(t, h, unscopedMount, sid, body)
}

// callTool returns the tool result's text and whether it was an error result.
func callTool(t *testing.T, h http.Handler, sid, name, args string) (string, bool) {
	t.Helper()
	return callToolAt(t, h, unscopedMount, sid, name, args)
}

// The whole unscoped-endpoint story over HTTP: nothing works until knomit_bind
// hands back a handle, the handle opens the tools, and a handle naming a
// subscription keeps reads working while writes are refused.
func TestSessionBoundMCP_EndToEnd(t *testing.T) {
	h, sessStore := sessionBoundE2E(t)

	// initialize: nothing is bound, and on this endpoint nothing ever is until
	// a call carries a handle.
	initResp, sid := rpc(t, h, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1.0"}}}`)
	require.NotEmpty(t, sid, "server must mint a session id")
	instr, _ := initResp["result"].(map[string]any)["instructions"].(string)
	require.Contains(t, instr, "knomit_bind", "unscoped initialize must say to bind first")
	require.Contains(t, instr, "`binding`", "and must name the argument that carries the handle")

	require.Equal(t, "", bindingOf(t, sessStore, sid),
		"initialize resolves no binding, so it stamps none")

	// knomit_repos needs NO handle: it is the discovery tool, and how the agent
	// learns the names knomit_bind accepts. It answers before anything is
	// bound, and the other seven tools fail closed until a handle exists.
	text, isErr := callTool(t, h, sid, "knomit_repos", `{}`)
	require.False(t, isErr, "knomit_repos must work with no handle: %s", text)
	require.Contains(t, text, `"name": "alpha"`)
	require.Contains(t, text, `"name": "followed"`)
	require.Contains(t, text, `"mode": "subscribe"`)
	require.NotContains(t, text, `"bound"`, "nothing is bound yet")
	require.NotContains(t, text, "uid-", "a registry uid must never reach a name or id field")

	// A gated tool fails closed, naming the tool to call and the argument.
	text, isErr = callTool(t, h, sid, "knomit_query", `{"text":"anything"}`)
	require.True(t, isErr, "knomit_query must fail with no handle: %s", text)
	require.Contains(t, text, "knomit_bind")
	require.Contains(t, text, "`binding`")

	// Bind the writable repo and keep the handle.
	alpha := bindHandle(t, h, sid, "alpha")

	// The pin the gate resolved lands on the client_sessions row for THIS
	// request — no lag, because the recorder carries it back out of the handler
	// rather than being read off a context built before the call.
	text, isErr = callTool(t, h, sid, "knomit_repos", fmt.Sprintf(`{"binding":%q}`, alpha))
	require.False(t, isErr, text)
	require.Equal(t, "repo:uid-alpha", bindingOf(t, sessStore, sid))

	var envelope struct {
		Repos []map[string]any `json:"repos"`
		Bound *struct {
			Name   string           `json:"name"`
			Mounts []map[string]any `json:"mounts"`
		} `json:"bound"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &envelope))
	require.Len(t, envelope.Repos, 2, "the catalogue is still there when bound")
	require.NotNil(t, envelope.Bound)
	require.Equal(t, "alpha", envelope.Bound.Name)
	require.Len(t, envelope.Bound.Mounts, 1)
	require.Equal(t, "read+write", envelope.Bound.Mounts[0]["role"])

	// A second bind mints a SECOND handle. The first keeps working — that is
	// the whole difference from the session-keyed upsert this replaced.
	followed := bindHandle(t, h, sid, "followed")
	require.NotEqual(t, alpha, followed)

	// A write through the subscription's handle is refused — by the binding's
	// own WriteOK, not by anything the gate added.
	text, isErr = callTool(t, h, sid, "knomit_learn", learnArgs(followed, "x/y", "t"))
	require.True(t, isErr, "a subscription must refuse writes: %s", text)
	require.Contains(t, text, "read-only view")

	// ...but reads still work on that same handle.
	text, isErr = callTool(t, h, sid, "knomit_query", fmt.Sprintf(`{"binding":%q,"text":"anything"}`, followed))
	require.False(t, isErr, "a subscription must still serve reads: %s", text)

	// And alpha's handle is untouched by the later bind.
	text, isErr = callTool(t, h, sid, "knomit_learn", learnArgs(alpha, "x/y", "t"))
	require.False(t, isErr, "the first handle must still write to alpha: %s", text)
	require.Contains(t, text, `"repo":"alpha"`,
		"the later bind must not have retargeted the earlier handle")
}

// The three refusals, over HTTP, asserted on their TEXT — an agent that cannot
// tell "you forgot the handle" from "that handle is not one of mine" from "this
// endpoint does not take one" retries the wrong repair forever.
func TestSessionBoundMCP_HandleErrors(t *testing.T) {
	h, _ := sessionBoundE2E(t)
	_, sid := rpc(t, h, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1.0"}}}`)

	// Missing.
	text, isErr := callTool(t, h, sid, "knomit_query", `{"text":"x"}`)
	require.True(t, isErr)
	require.Contains(t, text, "knomit_bind")
	require.Contains(t, text, "`binding`")
	require.Contains(t, text, "knomit_query")

	// Unknown — including the name, which is the thing a model will reach for.
	for _, bad := range []string{"never-minted", "alpha", "repo:uid-alpha"} {
		text, isErr = callTool(t, h, sid, "knomit_query",
			fmt.Sprintf(`{"binding":%q,"text":"x"}`, bad))
		require.True(t, isErr, "handle %q was accepted", bad)
		require.Equal(t, "unknown binding handle — call knomit_bind", text)
	}

	// Every gated tool answers the same way, so an agent cannot learn a wrong
	// lesson from whichever one it happened to call first.
	for tool, args := range map[string]string{
		"knomit_query":       `{"text":"x"}`,
		"knomit_explain":     `{"file":"kb/architecture/x/1.md"}`,
		"knomit_changes":     `{"prefix":"tasks"}`,
		"knomit_learn":       `{"moment_name":"m","facts":[{"topic":"architecture","category":"x/y","title":"t","body":"b"}]}`,
		"knomit_update":      `{"file":"kb/architecture/x/1.md"}`,
		"knomit_retract":     `{"file":"kb/architecture/x/1.md"}`,
		"knomit_review":      `{}`,
		"knomit_hypothesize": `{}`,
	} {
		text, isErr = callTool(t, h, sid, tool, args)
		require.True(t, isErr, "%s ran with no handle: %s", tool, text)
		require.Contains(t, text, "knomit_bind", tool)
	}
}

// A handle on a URL-scoped endpoint is refused rather than ignored: the caller
// believes it selected a repo, and the URL would have served a different one.
func TestURLScopedMCP_RefusesABindingArgument(t *testing.T) {
	h, _ := sessionBoundE2E(t)
	const mount = "/repos/alpha/branches/agent%2Ftest/mcp"

	_, sid := rpcAt(t, h, mount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1.0"}}}`)

	text, isErr := callToolAt(t, h, mount, sid, "knomit_query", `{"binding":"anything","text":"x"}`)
	require.True(t, isErr, "a handle on a URL-scoped endpoint must fail: %s", text)
	require.Equal(t, "this endpoint is bound by its URL; do not pass binding", text)

	// knomit_repos gives the same answer — the mistake is the same one.
	text, isErr = callToolAt(t, h, mount, sid, "knomit_repos", `{"binding":"anything"}`)
	require.True(t, isErr, text)
	require.Equal(t, "this endpoint is bound by its URL; do not pass binding", text)

	// Without one, the URL-scoped endpoint works exactly as before.
	text, isErr = callToolAt(t, h, mount, sid, "knomit_repos", `{}`)
	require.False(t, isErr, text)
	require.Contains(t, text, `"name": "alpha"`)
}

// bindingOf returns the binding column of one client_sessions row, or "" when
// the row does not exist yet.
func bindingOf(t *testing.T, st *sessions.Store, sid string) string {
	t.Helper()
	rows, _, err := st.List(context.Background(), sessions.Filter{Now: time.Now(), IncludeHidden: true})
	require.NoError(t, err)
	for _, r := range rows {
		if r.ID == sid {
			return r.Binding
		}
	}
	return ""
}
