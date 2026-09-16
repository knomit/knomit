package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"knomit/internal/client/sessions"
	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// bindFixture stands up a started Manager carrying a normal repo, a
// subscription, and a lens over two repos, with a real sessions store attached.
func bindFixture(t *testing.T) (*repos.Manager, *sessions.Store, *mcpserver.MCPServer) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir()},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	st := newSessionsStore(t)
	m.SetClientSessions(st)

	newBindRepo(t, m, "alpha", false)
	newBindRepo(t, m, "beta", false)
	newBindRepo(t, m, "followed", true)

	_, err := m.CreateLens(context.Background(), repos.Lens{
		Name:     "eng",
		WriteUID: "uid-alpha",
		Reads:    []repos.LensRead{{RepoUID: "uid-beta"}},
	})
	require.NoError(t, err)

	return m, st, NewServer("kb", m, false)
}

// newBindRepo registers a repo instance on m. A subscribed one carries no
// agent branch and reads the upstream it follows — the paired state the
// subscription invariant requires.
func newBindRepo(t *testing.T, m *repos.Manager, name string, subscribed bool) *repos.RepoInstance {
	t.Helper()
	dir := t.TempDir()
	branch := "agent/test"
	if subscribed {
		branch = "main"
	}
	svc, err := store.Open(filepath.Join(dir, "k.db"))
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
	ri := repos.NewTestInstanceWithDeps(cfg)
	m.Set(name, ri)
	return ri
}

// callBind invokes knomit_bind on a session-scoped context carrying sid.
func callBind(t *testing.T, m *repos.Manager, srv *mcpserver.MCPServer, sid string, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	ctx := repos.WithSessionScoped(context.Background())
	if sid != "" {
		ctx = srv.WithContext(ctx, &fakeSession{id: sid})
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := BindHandler(m)(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func TestBind_RepoWritable(t *testing.T) {
	m, st, srv := bindFixture(t)
	res := callBind(t, m, srv, "sid-1", map[string]any{"repo": "alpha"})
	require.False(t, res.IsError, resultText(t, res))

	text := resultText(t, res)
	require.Contains(t, text, `"role": "read+write"`)
	require.Contains(t, text, `"write_branch": "agent/test"`)
	require.Contains(t, text, "## Ontology Structure", "the bound base's instructions ride back")

	pin, ok, err := st.SessionBinding(context.Background(), "sid-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "repo:uid-alpha", pin, "stored by uid, so a rename cannot strand the session")
}

// A subscription binds at the branch it FOLLOWS and is read-only — reads work,
// writes are refused by the binding's own WriteOK, not by anything here.
func TestBind_SubscriptionReadOnly(t *testing.T) {
	m, st, srv := bindFixture(t)
	res := callBind(t, m, srv, "sid-2", map[string]any{"repo": "followed"})
	require.False(t, res.IsError, resultText(t, res))

	text := resultText(t, res)
	require.Contains(t, text, `"role": "read"`)
	require.Contains(t, text, `"mode": "subscribe"`)
	require.Contains(t, text, `"branch": "main"`)
	require.NotContains(t, text, `"write_branch"`)

	pin, _, _ := st.SessionBinding(context.Background(), "sid-2")
	require.Equal(t, "repo:uid-followed", pin)
}

func TestBind_Lens(t *testing.T) {
	m, st, srv := bindFixture(t)
	res := callBind(t, m, srv, "sid-3", map[string]any{"lens": "eng"})
	require.False(t, res.IsError, resultText(t, res))

	text := resultText(t, res)
	require.Contains(t, text, `"binding": "eng"`)
	require.Contains(t, text, "### Mounts", "a lens binding carries the mount table")

	pin, ok, _ := st.SessionBinding(context.Background(), "sid-3")
	require.True(t, ok)
	require.True(t, strings.HasPrefix(pin, "lens:"), "pin=%q", pin)
}

func TestBind_RebindSwitches(t *testing.T) {
	m, st, srv := bindFixture(t)
	require.False(t, callBind(t, m, srv, "sid-4", map[string]any{"repo": "alpha"}).IsError)
	require.False(t, callBind(t, m, srv, "sid-4", map[string]any{"repo": "beta"}).IsError)

	pin, _, _ := st.SessionBinding(context.Background(), "sid-4")
	require.Equal(t, "repo:uid-beta", pin, "a session switches; the second bind overwrites")
}

// Neither argument is an argument error, NOT a clear: there is no unbind form.
func TestBind_ArgErrors(t *testing.T) {
	m, _, srv := bindFixture(t)

	res := callBind(t, m, srv, "sid-5", map[string]any{})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "repo")
	require.Contains(t, resultText(t, res), "lens")

	res = callBind(t, m, srv, "sid-5", map[string]any{"repo": "alpha", "lens": "eng"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "not both")
}

func TestBind_UnknownName(t *testing.T) {
	m, _, srv := bindFixture(t)

	res := callBind(t, m, srv, "sid-6", map[string]any{"repo": "zzz"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `no repo named "zzz"`)

	res = callBind(t, m, srv, "sid-6", map[string]any{"lens": "zzz"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `no lens named "zzz"`)
}

// On a URL-scoped mount there is nothing to switch, and the error says where
// to connect instead.
func TestBind_RefusedOnURLScopedMount(t *testing.T) {
	m, _, srv := bindFixture(t)
	ctx := srv.WithContext(context.Background(), &fakeSession{id: "sid-7"})
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"repo": "alpha"}

	res, err := BindHandler(m)(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "/api/v1/mcp")
}

func TestBind_NoSessionID(t *testing.T) {
	m, _, srv := bindFixture(t)
	res := callBind(t, m, srv, "", map[string]any{"repo": "alpha"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "session id")
}
