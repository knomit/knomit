package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"

	"knomit/internal/corpus/repos/control"
	"knomit/internal/foundation/clientsessions"
)

// spyHandler records the context and request the gate let through.
type spyHandler struct {
	called bool
	ctx    context.Context
}

func (s *spyHandler) fn(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	s.called = true
	s.ctx = ctx
	return mcpgo.NewToolResultText("ok"), nil
}

// gateCall runs one call through the gate for queryTool (a gateRequired tool).
func gateCall(t *testing.T, m *repos.Manager, ctx context.Context, args any) (*mcpgo.CallToolResult, *spyHandler) {
	t.Helper()
	spy := &spyHandler{}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := gateBinding(m, queryTool(), gateRequired, spy.fn)(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res, spy
}

// mintHandle binds repo through knomit_bind and returns the handle.
func mintHandle(t *testing.T, m *repos.Manager, repo string) string {
	t.Helper()
	ctx := repos.WithSessionScoped(context.Background())
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"repo": repo}
	res, err := BindHandler(m)(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	return handleFrom(t, res)
}

// No handle on the unscoped endpoint: the call never runs, and the error names
// both the tool that mints a handle and the argument that carries it.
func TestGate_MissingHandleIsRefused(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())

	for name, args := range map[string]any{
		"no arguments at all": nil,
		"other arguments":     map[string]any{"text": "anything"},
		"empty handle":        map[string]any{"binding": "", "text": "anything"},
	} {
		t.Run(name, func(t *testing.T) {
			res, spy := gateCall(t, m, ctx, args)
			require.True(t, res.IsError)
			require.False(t, spy.called, "the handler must not run")
			text := resultText(t, res)
			require.Contains(t, text, "knomit_bind")
			require.Contains(t, text, "`binding`")
			require.Contains(t, text, "knomit_query", "the error must name the tool that refused")
		})
	}
}

// A handle nobody minted is refused with a fixed message that says nothing
// about which handles DO exist — the handle is the only thing separating two
// callers on one connection, so enumerating live ones would hand one caller
// another's routing.
func TestGate_UnknownHandleIsRefused(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())
	live := mintHandle(t, m, "alpha")

	for _, bad := range []string{"not-a-handle", "alpha", "repo:uid-alpha", live + "x"} {
		res, spy := gateCall(t, m, ctx, map[string]any{"binding": bad})
		require.True(t, res.IsError, "handle %q was accepted", bad)
		require.False(t, spy.called)
		require.Equal(t, errUnknownHandle, resultText(t, res))
		require.NotContains(t, resultText(t, res), live, "must not echo a live handle")
		require.NotContains(t, resultText(t, res), "alpha", "must not echo what exists")
	}
}

// The repo or lens NAME is not a handle. Stated separately because it is the
// single most likely thing a model will try, and a design where it worked would
// be the session-keyed bug wearing an argument.
func TestGate_TheNameIsNotAHandle(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())
	_ = mintHandle(t, m, "alpha")

	res, spy := gateCall(t, m, ctx, map[string]any{"binding": "alpha", "text": "x"})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Equal(t, errUnknownHandle, resultText(t, res))
}

// A good handle reaches the handler as a context in exactly the shape the
// middleware used to build: the Binding, plus the write repo as the context
// RepoInstance. That is what lets every handler body stay untouched.
func TestGate_ResolvedHandleReachesTheHandler(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())
	ctx = repos.WithPinRecorder(ctx, &repos.PinRecorder{})

	res, spy := gateCall(t, m, ctx, map[string]any{"binding": mintHandle(t, m, "alpha"), "text": "x"})
	require.False(t, res.IsError, resultText(t, res))
	require.True(t, spy.called)

	b, err := repos.RequireBinding(spy.ctx)
	require.NoError(t, err)
	require.Equal(t, "repo:uid-alpha", b.PinID())
	ri, ok := repos.RepoFromContextOpt(spy.ctx)
	require.True(t, ok)
	require.Equal(t, "alpha", ri.Name())

	// And the pin is reported for the observational client_sessions column.
	rec, ok := repos.PinRecorderFromContext(spy.ctx)
	require.True(t, ok)
	// The HANDLE is recorded alongside the pin: the session's binding set is
	// keyed by handle, so a recorder that reported only the pin would collapse
	// two callers back into one.
	resolved := rec.Resolved()
	require.Equal(t, "repo:uid-alpha", resolved.Pin)
	require.NotEmpty(t, resolved.Handle)
	require.Equal(t, "", resolved.Branch, `"" means the target's own read branch`)
}

// TWO handles resolve independently in the SAME process with no session state
// between them — the unit-level statement of the incident.
func TestGate_TwoHandlesResolveIndependently(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())
	hA, hB := mintHandle(t, m, "alpha"), mintHandle(t, m, "beta")

	// Interleaved on purpose: A, then B, then A again. The middle call must not
	// change what A's handle means — that interleaving IS the incident.
	for _, step := range []struct{ handle, want string }{
		{hA, "alpha"}, {hB, "beta"}, {hA, "alpha"}, {hB, "beta"},
	} {
		_, spy := gateCall(t, m, ctx, map[string]any{"binding": step.handle})
		require.True(t, spy.called)
		b, err := repos.RequireBinding(spy.ctx)
		require.NoError(t, err)
		require.Equal(t, step.want, b.Name())
	}
}

// A handle whose repo has gone surfaces the RESOLVER's reason, not the generic
// unknown-handle text: "bind again" is useless advice if the agent cannot tell
// that the base it chose no longer exists.
func TestGate_MintedHandleWhoseRepoIsGone(t *testing.T) {
	m, st, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())

	handle, err := sessionsNewHandle()
	require.NoError(t, err)
	require.NoError(t, st.MintBindingHandle(context.Background(), handle, "repo:uid-departed", "", time.Now()))

	res, spy := gateCall(t, m, ctx, map[string]any{"binding": handle})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.NotEqual(t, errUnknownHandle, resultText(t, res),
		"a minted handle pointing at a dead repo is a different answer from an unknown handle")
	require.Contains(t, resultText(t, res), "not available")
	require.Contains(t, resultText(t, res), "knomit_bind")
}

// scopedCtx is a URL-scoped request for repo alpha at its agent branch: a
// RepoInstance, the URL branch, the mount marker MountExperimentMiddleware sets
// on a mount a handle may steer, and NO session-scoped marker.
func scopedCtx(t *testing.T, m *repos.Manager, repo string) context.Context {
	t.Helper()
	ri := m.Get(repo)
	require.NotNil(t, ri)
	ctx := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), ri.AgentBranch())
	return repos.WithMountSession(ctx, "sid-scoped", repos.PinForRepo(ri))
}

// mintScopedHandle binds through knomit_bind on a scoped context.
func mintScopedHandle(t *testing.T, m *repos.Manager, ctx context.Context) string {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{}
	res, err := BindHandler(m)(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	return handleFrom(t, res)
}

// On a URL-scoped endpoint `binding` is OPTIONAL. Absent, or "", is the URL's
// own binding exactly as before; a handle minted on this mount routes the call
// and leaves the mount's session state behind; anything else is refused, never
// ignored.
func TestGate_ScopedEndpointHandles(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := scopedCtx(t, m, "alpha")

	for name, args := range map[string]any{
		"no key":   map[string]any{"text": "x"},
		"an empty": map[string]any{"binding": "", "text": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			res, spy := gateCall(t, m, ctx, args)
			require.False(t, res.IsError, resultText(t, res))
			require.True(t, spy.called)
			b, err := repos.RequireBinding(spy.ctx)
			require.NoError(t, err)
			require.Equal(t, "alpha", b.Name())
			require.Empty(t, bindingHandleFromContext(spy.ctx))
			_, _, plain := repos.MountSessionFromContext(spy.ctx)
			require.True(t, plain, "a handle-less call keeps the mount's session state")
		})
	}

	handle := mintScopedHandle(t, m, ctx)
	res, spy := gateCall(t, m, ctx, map[string]any{"binding": handle, "text": "x"})
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, handle, bindingHandleFromContext(spy.ctx))
	_, _, plain := repos.MountSessionFromContext(spy.ctx)
	require.False(t, plain, "a handle-routed call must not see the connection's session state")

	res, spy = gateCall(t, m, ctx, map[string]any{"binding": "never-minted", "text": "x"})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Equal(t, errUnknownHandle, resultText(t, res))

	// A mount whose URL names a specific branch carries no marker: no handle
	// can steer it.
	fixed := repos.WithBranch(repos.WithRepoInstance(context.Background(), m.Get("alpha")), "main")
	res, spy = gateCall(t, m, fixed, map[string]any{"binding": handle, "text": "x"})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Contains(t, resultText(t, res), `fixed to repo alpha at branch main`)
}

// A handle works only on the endpoint that minted it: an unscoped handle for
// the same repo is refused on the scoped mount, and the scoped one on the
// unscoped endpoint.
func TestGate_HandleIsConfinedToItsEndpoint(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := scopedCtx(t, m, "alpha")
	unscoped := mintHandle(t, m, "alpha")
	scoped := mintScopedHandle(t, m, ctx)

	res, spy := gateCall(t, m, ctx, map[string]any{"binding": unscoped, "text": "x"})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Contains(t, resultText(t, res), "belongs to another endpoint")

	res, spy = gateCall(t, m, scopedCtx(t, m, "beta"), map[string]any{"binding": scoped, "text": "x"})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Contains(t, resultText(t, res), `this endpoint's repo beta`)

	res, spy = gateCall(t, m, repos.WithSessionScoped(context.Background()), map[string]any{"binding": scoped, "text": "x"})
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Equal(t, errHandleFromScopedEndpoint, resultText(t, res))
}

// A non-object `arguments` keeps the handler's own message. Read as an absent
// handle it would send the caller to knomit_bind to fix a malformed envelope.
func TestGate_NonObjectArgumentsKeepTheirOwnError(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())

	spy := &spyHandler{}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = json.RawMessage(`"a string, not an object"`)
	res, err := gateBinding(m, queryTool(), gateRequired, spy.fn)(ctx, req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.False(t, spy.called)
	require.Contains(t, resultText(t, res), "expected a JSON object")
}

// knomit_repos is the discovery tool: it answers with NO handle, and REPORTS a
// bad one instead of raising, because it is the tool an agent calls to find out
// what went wrong.
func TestGate_ReposIsOptionalAndReportsRatherThanRaises(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())
	gated := gateBinding(m, reposTool(), gateOptional, ReposHandler(m))

	// No handle: the catalogue, no `bound`.
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{}
	res, err := gated(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	var envelope reposResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &envelope))
	require.NotEmpty(t, envelope.Repos)
	require.Nil(t, envelope.Bound, "nothing is bound")

	// A good handle: `bound` describes what it names.
	req.Params.Arguments = map[string]any{"binding": mintHandle(t, m, "alpha")}
	res, err = gated(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &envelope))
	require.NotNil(t, envelope.Bound)
	require.Equal(t, "alpha", envelope.Bound.Name)
	require.Len(t, envelope.Bound.Mounts, 1)

	// An unknown handle: reported under `bound`, not raised.
	req.Params.Arguments = map[string]any{"binding": "never-minted"}
	res, err = gated(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "knomit_repos must still answer: %s", resultText(t, res))
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &envelope))
	require.NotNil(t, envelope.Bound)
	require.Equal(t, "unresolvable", envelope.Bound.Status)
	require.Contains(t, envelope.Bound.Error, "knomit_bind")
	require.NotEmpty(t, envelope.Repos, "the catalogue survives a bad handle")
}

// A handle whose repo is gone is reported by knomit_repos with the kind and the
// name the registry still knows — the pre-existing `unresolvable` contract,
// reached now through a handle rather than a stored session pin.
func TestGate_ReposReportsUnresolvableWithKindAndName(t *testing.T) {
	m, st, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())

	// A repo that is registered but has no live instance.
	require.NoError(t, m.Repos().Insert(control.RepoRecord{
		UID: "uid-departed", Name: "departed", State: control.StateActive, Profile: "code", CreatedAt: 1,
	}))
	handle, err := sessionsNewHandle()
	require.NoError(t, err)
	require.NoError(t, st.MintBindingHandle(context.Background(), handle, "repo:uid-departed", "", time.Now()))

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"binding": handle}
	res, err := gateBinding(m, reposTool(), gateOptional, ReposHandler(m))(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))

	var envelope reposResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &envelope))
	require.NotNil(t, envelope.Bound)
	require.Equal(t, "unresolvable", envelope.Bound.Status)
	require.Equal(t, "repo", envelope.Bound.Kind)
	require.Equal(t, "departed", envelope.Bound.Name)
	require.NotContains(t, envelope.Bound.Name, "uid-", "a uid must never reach a name field")
}

// knomit_repos ALWAYS answers on a URL-scoped endpoint: a bad handle is
// reported under `bound`, exactly as on the unscoped endpoint, and the
// connection's fixed target is in `endpoint`.
func TestGate_ReposAnswersOnURLScopedEndpoint(t *testing.T) {
	m, _, _ := bindFixture(t)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"binding": "anything"}
	res, err := gateBinding(m, reposTool(), gateOptional, ReposHandler(m))(scopedCtx(t, m, "alpha"), req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))

	var envelope reposResponse
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &envelope))
	require.NotNil(t, envelope.Bound)
	require.Equal(t, "unresolvable", envelope.Bound.Status)
	require.NotNil(t, envelope.Endpoint)
	require.Equal(t, "alpha", envelope.Endpoint.Name)
	require.NotEmpty(t, envelope.Repos)
}

// EVERY tool that operates on a knowledge base declares `binding`, and the two
// discovery tools are the deliberate exceptions. A new tool that forgets the
// argument would be permanently uncallable on the unscoped endpoint — the gate
// would demand a handle its schema does not accept, and rejectUnknownArguments
// would refuse the handle. This is the guard that notices.
func TestToolRegistrations_GateAndSchemaAgree(t *testing.T) {
	m, _, _ := bindFixture(t)
	for _, reg := range toolRegistrations(m) {
		_, declared := reg.tool.InputSchema.Properties[bindingArgument]
		switch reg.gate {
		case gateUngated:
			require.Equal(t, "knomit_bind", reg.tool.Name,
				"only knomit_bind may skip the gate")
			require.False(t, declared, "knomit_bind must not declare a binding argument")
		case gateOptional:
			require.Equal(t, "knomit_repos", reg.tool.Name,
				"only knomit_repos may treat the handle as optional")
			require.True(t, declared)
		case gateRequired:
			require.True(t, declared,
				"%s is gated on a handle it does not declare, so no call could ever pass",
				reg.tool.Name)
		}
	}
}

// sessionsNewHandle mints a handle the way knomit_bind does, for the tests that
// need to plant one pointing somewhere knomit_bind would never have bound.
func sessionsNewHandle() (string, error) { return clientsessions.NewBindingHandle() }

// A handle of the wrong TYPE is named as such rather than folded into "you
// forgot it" — a caller that sent a number would read that advice as already
// followed and resend the same shape.
func TestGate_NonStringHandleNamesTheType(t *testing.T) {
	m, _, _ := bindFixture(t)
	ctx := repos.WithSessionScoped(context.Background())

	for name, v := range map[string]any{
		"a number":  42,
		"an array":  []any{"a"},
		"an object": map[string]any{"handle": "x"},
		"null":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			res, spy := gateCall(t, m, ctx, map[string]any{"binding": v})
			require.True(t, res.IsError)
			require.False(t, spy.called)
			require.Contains(t, resultText(t, res), "`binding` must be the handle string")
		})
	}
}
