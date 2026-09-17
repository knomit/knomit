package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/client/sessions"
	"knomit/internal/repos"
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

// On a URL-scoped endpoint the repo comes from the path. A handle there is
// refused on PRESENCE — silently ignoring it would let a caller believe it
// selected one repo while the URL served another.
func TestGate_HandleOnURLScopedEndpointIsRefused(t *testing.T) {
	m, _, _ := bindFixture(t)
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	// A URL-scoped context: a RepoInstance, and NO session-scoped marker.
	urlCtx := repos.WithRepoInstance(context.Background(), ri)

	for name, args := range map[string]any{
		"a real handle":  map[string]any{"binding": "anything-at-all", "text": "x"},
		"an empty value": map[string]any{"binding": "", "text": "x"},
	} {
		t.Run(name, func(t *testing.T) {
			res, spy := gateCall(t, m, urlCtx, args)
			require.True(t, res.IsError)
			require.False(t, spy.called)
			require.Equal(t, errBindingOnURLScoped, resultText(t, res))
		})
	}

	// Without one, the URL-scoped call passes through untouched.
	res, spy := gateCall(t, m, urlCtx, map[string]any{"text": "x"})
	require.False(t, res.IsError, resultText(t, res))
	require.True(t, spy.called)
	b, err := repos.RequireBinding(spy.ctx)
	require.NoError(t, err)
	require.Equal(t, "alpha", b.Name())
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
	require.NoError(t, m.Repos().Insert(repos.RepoRecord{
		UID: "uid-departed", Name: "departed", State: repos.StateActive, Profile: "code", CreatedAt: 1,
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

// A `binding` on a URL-scoped endpoint fails for knomit_repos too — the same
// answer every other tool gives, because the mistake is the same one.
func TestGate_ReposRefusesHandleOnURLScopedEndpoint(t *testing.T) {
	m, _, _ := bindFixture(t)
	ri := m.Get("alpha")
	require.NotNil(t, ri)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"binding": "anything"}
	res, err := gateBinding(m, reposTool(), gateOptional, ReposHandler(m))(
		repos.WithRepoInstance(context.Background(), ri), req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, errBindingOnURLScoped, resultText(t, res))
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
func sessionsNewHandle() (string, error) { return sessions.NewBindingHandle() }

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
