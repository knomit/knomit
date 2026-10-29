package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/clientsessions"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// bindFixture stands up a started Manager carrying a normal repo, a
// subscription, and a lens over two repos, with a real sessions store attached.
func bindFixture(t *testing.T) (*repos.Manager, *clientsessions.Store, *mcpserver.MCPServer) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: t.TempDir()},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	st := newSessionsStore(t)
	m.SetClientSessions(st)

	newBindRepo(t, m, "alpha", false)
	newBindRepo(t, m, "beta", false)
	newBindRepo(t, m, "followed", true)

	_, err := m.CreateLens(context.Background(), control.Lens{
		Name:     "eng",
		WriteUID: "uid-alpha",
		Reads:    []control.LensRead{{RepoUID: "uid-beta"}},
	})
	require.NoError(t, err)

	return m, st, NewServer("kb", m, false, nil)
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
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, branch))

	uid := "uid-" + name
	require.NoError(t, m.Repos().Insert(control.RepoRecord{
		UID: uid, Name: name, State: control.StateActive, Profile: "code", CreatedAt: 1,
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

// callBind invokes knomit_bind on a session-scoped context. sid is threaded
// only to prove it is IGNORED — knomit_bind reads no session id, which is what
// lets two callers sharing one connection bind independently.
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

// handleFrom pulls the minted handle out of a knomit_bind result: the leading
// JSON object, before the prose.
func handleFrom(t *testing.T, res *mcpgo.CallToolResult) string {
	t.Helper()
	var envelope struct {
		Binding string `json:"binding"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(resultText(t, res))).Decode(&envelope))
	require.NotEmpty(t, envelope.Binding)
	return envelope.Binding
}

// pinOfHandle reads what a handle routes to.
func pinOfHandle(t *testing.T, st *clientsessions.Store, handle string) string {
	t.Helper()
	pin, branch, ok, err := st.BindingHandle(context.Background(), handle, time.Now())
	require.NoError(t, err)
	require.True(t, ok, "handle %q was not recorded", handle)
	require.Equal(t, "", branch, `knomit_bind stores no branch yet; "" means the target's own`)
	return pin
}

func TestBind_RepoWritable(t *testing.T) {
	m, st, srv := bindFixture(t)
	res := callBind(t, m, srv, "sid-1", map[string]any{"repo": "alpha"})
	require.False(t, res.IsError, resultText(t, res))

	text := resultText(t, res)
	require.Contains(t, text, `"role": "read+write"`)
	// knomit_bind returns the mount table it just made, NOT the whole
	// catalogue: the agent asked to bind, not to browse.
	require.NotContains(t, text, `"repos":`)
	require.NotContains(t, text, `"lenses":`)
	require.Contains(t, text, `"write_branch": "agent/test"`)
	require.Contains(t, text, "## Ontology Structure", "the bound base's instructions ride back")

	// The handle comes FIRST, and the instruction to resend it comes before the
	// knowledge base's own instructions: an agent that reads only the opening
	// of a long result must still come away knowing it has to carry this value.
	require.Regexp(t, `\A\s*\{\s*"binding":`, text, "the handle must be the first key")
	require.Less(t, strings.Index(text, "## Your binding handle"),
		strings.Index(text, "## Ontology Structure"),
		"the handle banner must precede the base's instructions")
	require.Contains(t, text, "`binding` argument")

	handle := handleFrom(t, res)
	require.NotContains(t, handle, "alpha", "a handle must not be derivable from the name")
	require.Equal(t, "repo:uid-alpha", pinOfHandle(t, st, handle),
		"stored by uid, so a rename cannot strand the handle")
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

	require.Equal(t, "repo:uid-followed", pinOfHandle(t, st, handleFrom(t, res)))
}

func TestBind_Lens(t *testing.T) {
	m, st, srv := bindFixture(t)
	res := callBind(t, m, srv, "sid-3", map[string]any{"lens": "eng"})
	require.False(t, res.IsError, resultText(t, res))

	text := resultText(t, res)
	// `name` is the lens name. `binding` is the HANDLE and never a name — one
	// key, one meaning, across knomit_bind and knomit_repos alike.
	require.Contains(t, text, `"name": "eng"`)
	require.NotContains(t, text, `"binding": "eng"`)
	require.Contains(t, text, "### Mounts", "a lens binding carries the mount table")

	pin := pinOfHandle(t, st, handleFrom(t, res))
	require.True(t, strings.HasPrefix(pin, "lens:"), "pin=%q", pin)
}

// THE INCIDENT, at the tool. Two binds on ONE session id mint two DIFFERENT
// handles, and the first keeps naming what it named. Under the session-keyed
// upsert this replaced, the second bind silently redirected the first caller's
// writes — which is exactly what happened to two Cowork jobs sharing one
// Claude Desktop connection on 2026-09-17.
func TestBind_SecondBindDoesNotStealTheFirstHandle(t *testing.T) {
	m, st, srv := bindFixture(t)
	first := callBind(t, m, srv, "sid-4", map[string]any{"repo": "alpha"})
	require.False(t, first.IsError, resultText(t, first))
	second := callBind(t, m, srv, "sid-4", map[string]any{"repo": "beta"})
	require.False(t, second.IsError, resultText(t, second))

	hA, hB := handleFrom(t, first), handleFrom(t, second)
	require.NotEqual(t, hA, hB, "each bind mints its own handle")
	require.Equal(t, "repo:uid-alpha", pinOfHandle(t, st, hA),
		"the second bind must not retarget the first handle")
	require.Equal(t, "repo:uid-beta", pinOfHandle(t, st, hB))
}

// The same session id is not merely tolerated, it is IRRELEVANT: binding twice
// under two different ids, or none at all, behaves identically.
func TestBind_SessionIDIsIgnored(t *testing.T) {
	m, st, srv := bindFixture(t)
	for _, sid := range []string{"sid-a", "sid-b", ""} {
		res := callBind(t, m, srv, sid, map[string]any{"repo": "alpha"})
		require.False(t, res.IsError, "sid=%q: %s", sid, resultText(t, res))
		require.Equal(t, "repo:uid-alpha", pinOfHandle(t, st, handleFrom(t, res)), "sid=%q", sid)
	}
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

// On a URL-scoped mount knomit_bind mints a handle on the URL's own repo that
// works only there; on a mount whose URL names a specific branch it refuses.
func TestBind_OnURLScopedMount(t *testing.T) {
	m, st, _ := bindFixture(t)
	ctx := scopedCtx(t, m, "alpha")

	handle := mintScopedHandle(t, m, ctx)
	require.Equal(t, "repo:uid-alpha", pinOfHandle(t, st, handle))
	mount, err := st.HandleMount(context.Background(), handle)
	require.NoError(t, err)
	require.Equal(t, "repo:uid-alpha", mount, "the handle is recorded against its mount")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"repo": "beta"}
	res, err := BindHandler(m)(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "This connection is fixed to repo alpha; knomit_bind here only changes the branch. "+
		"To use another repo, the user must restart the bridge with --repo beta. Do not retry.", resultText(t, res))

	fixed := repos.WithBranch(repos.WithRepoInstance(context.Background(), m.Get("alpha")), "main")
	req.Params.Arguments = map[string]any{}
	res, err = BindHandler(m)(fixed, req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "Do not retry")
}

// knomit_bind is where handles come from, so it declares no `binding` argument
// and rejectUnknownArguments refuses one — the right answer for an agent that
// has started attaching its handle reflexively to every call.
func TestBind_RejectsABindingArgument(t *testing.T) {
	m, _, srv := bindFixture(t)
	res := callBind(t, m, srv, "sid-8", map[string]any{"repo": "alpha", "binding": "whatever"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "binding")
}

// scopedBindTarget resolves the URL's repo or lens BY NAME, and the name can
// have come to mean another base since the URL resolved it: the lens was
// deleted and recreated under the same name, so the same URL now has a new uid.
// The bind must then be refused, not minted: a handle pinned to the new uid on
// the old mount would name a base the URL did not resolve to.
func TestScopedBindTarget_RefusesWhenTheNameNowNamesAnotherBase(t *testing.T) {
	m, _, _ := bindFixture(t)
	reg := m.LensRegistry()
	old, ok, err := reg.Get("eng")
	require.NoError(t, err)
	require.True(t, ok)
	oldB, err := repos.NewBindingOfLens(m, old)
	require.NoError(t, err)
	// The request as the lens route left it, resolved BEFORE the recreate.
	ctx := repos.WithMountSession(repos.WithBinding(context.Background(), oldB), "sid-scoped", oldB.PinID())

	_, pin, mount, err := scopedBindTarget(ctx, m, "", "")
	require.NoError(t, err, "before the recreate the name and the URL agree")
	require.Equal(t, oldB.PinID(), pin)
	require.Equal(t, pin, mount)

	require.NoError(t, reg.Delete("eng"))
	recreated, err := reg.Create(control.Lens{Name: "eng", WriteUID: "uid-alpha", Reads: []control.LensRead{{RepoUID: "uid-beta"}}})
	require.NoError(t, err)
	require.NotEqual(t, old.UID, recreated.UID, "the fixture must give the recreated lens a new uid")

	_, _, _, err = scopedBindTarget(ctx, m, "", "")
	require.EqualError(t, err, "lens eng was renamed or replaced while binding; call knomit_bind again")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{}
	res, err := BindHandler(m)(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError, "knomit_bind must refuse, got %s", resultText(t, res))
	require.Equal(t, "lens eng was renamed or replaced while binding; call knomit_bind again", resultText(t, res))
}
