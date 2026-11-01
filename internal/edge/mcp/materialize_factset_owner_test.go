package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/foundation/auth"
)

// A FactSet belongs to the principal ALONE (user ruling 2026-11-01): the same
// principal pages it from another MCP endpoint with the same binding, but
// not through a binding with another pin over the SAME session store and the
// SAME read set (a lens over alpha alone: pin lens:<uid>, not repo:<uid>),
// and not as another principal; both refusals are the unknown id's text, and
// a close through either closes nothing. A single-fact cursor keeps its
// endpoint + principal owner: another endpoint cannot resume it.
//
// Sabotage: put callerOwner back into factsetScope (the second endpoint gets
// the expired text); drop the pin from CloseFactSet's DELETE (the lens close
// closes it) or from openFactSet's row check (the lens pages it); use the
// principal alone for single-fact cursors (the second endpoint resumes it).
func TestFactSet_OwnerIsThePrincipalNotTheEndpoint(t *testing.T) {
	m, alpha, _, _, _, _ := newLensE2E(t)
	obs(t, alpha, "kb/u/r1.md", "R1")
	obs(t, alpha, "kb/u/r2.md", "R2", "kb/u/r1.md")
	obs(t, alpha, "kb/u/r3.md", "R3")

	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	desktop := auth.Principal{Kind: auth.KindHost, ID: "desktop", Via: auth.ViaToken}
	repoCtx := func(p auth.Principal, endpoint string) context.Context {
		ctx := auth.WithRepoReach(auth.WithPrincipal(context.Background(), p), auth.AllRepos)
		return WithEndpoint(repos.WithBinding(ctx, repos.NewBindingOfRepo(alpha, "agent/test")), endpoint)
	}
	scoped := repoCtx(laptop, "/api/v1/repos/alpha/branches/agent:test/mcp")
	unscoped := repoCtx(laptop, "/api/v1/mcp")
	stranger := repoCtx(desktop, "/api/v1/repos/alpha/branches/agent:test/mcp")
	// A lens over alpha alone: the SAME read set as the repo binding (alpha
	// at agent/test), another pin (lens:<uid>), the same session store. Only
	// the pin tells it from the repo binding.
	_, err := m.CreateLens(context.Background(), control.Lens{Name: "solo", WriteUID: alpha.UID()})
	require.NoError(t, err)
	const lens = "solo"
	lensCtx, err := repos.ResolveLensBinding(
		WithEndpoint(auth.WithRepoReach(auth.WithPrincipal(context.Background(), laptop), auth.AllRepos), "/api/v1/lenses/"+lens+"/mcp"),
		m, lens)
	require.NoError(t, err)
	lb, _ := repos.BindingFromContextOpt(lensCtx)
	rb, _ := repos.BindingFromContextOpt(scoped)
	require.NotEqual(t, rb.PinID(), lb.PinID(), "the lens is another pin")
	require.Equal(t, rb.Write().Name(), lb.Write().Name(), "over the same write repo, so the same session store")
	require.Equal(t, federate.ReadSetFingerprint(rb), federate.ReadSetFingerprint(lb), "and the same read set: only the pin differs")

	call := func(ctx context.Context, args map[string]any) (string, bool) {
		t.Helper()
		res, text := matCallRaw(t, ctx, m, args)
		return text, res.IsError
	}
	first, isErr := call(scoped, map[string]any{"search": map[string]any{"path": "kb/u/"}, "depth": 0, "limit": 1})
	require.False(t, isErr, first)
	var p matPage
	require.NoError(t, json.Unmarshal([]byte(first), &p), first)
	require.NotNil(t, p.Cursor)
	cur := *p.Cursor

	here, isErr := call(scoped, map[string]any{"cursor": cur})
	require.False(t, isErr, here)
	there, isErr := call(unscoped, map[string]any{"cursor": cur})
	require.False(t, isErr, "another endpoint, same principal and binding: %s", there)
	require.Equal(t, here, there, "the same page from either endpoint")

	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	for name, ctx := range map[string]context.Context{"lens pin": lensCtx, "another principal": stranger} {
		none, noneErr := call(ctx, map[string]any{"cursor": ghost})
		got, gotErr := call(ctx, map[string]any{"cursor": cur})
		require.True(t, noneErr, none)
		require.True(t, gotErr, "%s: %s", name, got)
		require.Equal(t, none, got, "%s: reads exactly like an unknown id", name)
		noneClose, _ := call(ctx, map[string]any{"close": true, "cursor": ghost})
		gotClose, closeErr := call(ctx, map[string]any{"close": true, "cursor": cur})
		require.True(t, closeErr, gotClose)
		require.Equal(t, noneClose, gotClose, "%s: closing reads like closing none", name)
		again, isErr := call(scoped, map[string]any{"cursor": cur})
		require.False(t, isErr, "%s closed nothing: %s", name, again)
	}

	// The owner closes it from the OTHER endpoint; then it is gone on both.
	closed, isErr := call(unscoped, map[string]any{"close": true, "cursor": cur})
	require.False(t, isErr, closed)
	require.JSONEq(t, `{"closed":true}`, closed)
	none, _ := call(scoped, map[string]any{"cursor": ghost})
	gone, isErr := call(scoped, map[string]any{"cursor": cur})
	require.True(t, isErr)
	require.Equal(t, none, gone)

	// A single-fact cursor is still owned by endpoint + principal.
	single := matCall(t, scoped, m, map[string]any{"file": "kb/u/r2.md", "audience": "human", "limit": 1})
	require.NotNil(t, single.Cursor)
	other, isErr := call(unscoped, map[string]any{"cursor": *single.Cursor})
	require.True(t, isErr, "another endpoint must not resume a single-fact cursor: %s", other)
	mine, isErr := call(scoped, map[string]any{"cursor": *single.Cursor})
	require.False(t, isErr, "the minting endpoint still resumes it: %s", mine)
}
