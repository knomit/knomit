package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// Lens FactSet CONTRACT tests (PR A, no behaviour change). They pin what a
// FactSet does through a lens endpoint TODAY, so the facade work cannot move
// it by accident. They are PARTLY SUPERSEDED when the lens facade lands (F3):
// the multi-member refusal below is replaced by the facade's own tests; the
// one-member path stays a contract unless the facade rules otherwise.

func factsetWrites(t *testing.T, ri *repos.RepoInstance) int64 {
	t.Helper()
	var n int64
	require.NoError(t, ri.WithRead(func(svc *store.Service) { n = svc.ToolSession().FactSetWrites() }))
	return n
}

func lensCtxFor(t *testing.T, m *repos.Manager, p auth.Principal, lens string) context.Context {
	t.Helper()
	ctx, err := repos.ResolveLensBinding(
		WithEndpoint(auth.WithRepoReach(auth.WithPrincipal(context.Background(), p), auth.AllRepos), "/api/v1/lenses/"+lens+"/mcp"),
		m, lens)
	require.NoError(t, err)
	return ctx
}

func repoCtxFor(ri *repos.RepoInstance, p auth.Principal) context.Context {
	return WithEndpoint(auth.WithRepoReach(auth.WithPrincipal(
		repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "agent/test")), p), auth.AllRepos),
		"/api/v1/repos/"+ri.Name()+"/branches/agent:test/mcp")
}

// B1 (MCP half). A factset call on a MULTI-member lens endpoint is refused by
// materialize.FactSetFirstPage's IsLens guard, surfaces as a tool error
// naming the lens, and creates no session row in either member's store.
// SUPERSEDED by the facade tests (F3), which lift this refusal.
//
// Sabotage: delete the IsLens refusal in FactSetFirstPage; the call then
// succeeds (a page, a row) and the IsError / row assertions fail.
func TestFactSet_Contract_MultiMemberLensIsRefusedWithNoSessionRow(t *testing.T) {
	m, alpha, beta, _, _, lens := newLensE2E(t)
	obs(t, alpha, "kb/u/a1.md", "A1")
	obs(t, beta, "kb/u/b1.md", "B1")
	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	ctx := lensCtxFor(t, m, laptop, lens)
	lb, ok := repos.BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.True(t, lb.IsLens(), "the fixture lens is a multi-member lens (alpha + beta)")
	require.Len(t, lb.Reads(), 2)

	wa, wb := factsetWrites(t, alpha), factsetWrites(t, beta)
	res, text := matCallRaw(t, ctx, m, map[string]any{"search": map[string]any{"path": "kb/u/"}, "depth": 0})
	require.True(t, res.IsError, "a factset on a lens endpoint is a tool error: %s", text)
	require.Contains(t, text, "a factset materializes one repository's search; this binding is a lens")
	require.Equal(t, wa, factsetWrites(t, alpha), "no session row in the write repo")
	require.Equal(t, wb, factsetWrites(t, beta), "no session row in the read repo")

	// The same search through a repo endpoint of the same write repo works:
	// the refusal is the lens guard, not the fixture.
	ok2, text2 := matCallRaw(t, repoCtxFor(alpha, laptop), m, map[string]any{"search": map[string]any{"path": "kb/u/"}, "depth": 0})
	require.False(t, ok2.IsError, text2)
	require.Equal(t, wa+1, factsetWrites(t, alpha), "the repo call wrote exactly one row")
}

// B3 (MCP half, Q-L9 ruling: keep allowing it, make it a contract). A lens
// whose only member is the write repo passes the IsLens guard (it federates
// across nothing) and behaves as the write repo's FactSet:
//   - the same roots, in the same order, the same units and the same bytes as
//     the repo endpoint's first page, except the cursor id;
//   - but its cursor is pinned lens:<uid>, so the repo endpoint (pin repo:<uid>)
//     cannot page it: it reads exactly like an unknown id.
//
// Partly superseded by the facade (F3), which may rule on the one-member lens.
//
// Sabotage: make IsLens true for a one-member lens (the lens call is refused,
// the page comparison fails); or give the lens the repo pin (the repo call
// pages the lens cursor and the refusal assertion fails).
func TestFactSet_Contract_OneMemberLensIsTheWriteRepoFactSetWithTheLensPin(t *testing.T) {
	m, alpha, _, _, _, _ := newLensE2E(t)
	obs(t, alpha, "kb/u/r1.md", "R1")
	obs(t, alpha, "kb/u/r2.md", "R2", "kb/u/r1.md")
	obs(t, alpha, "kb/u/r3.md", "R3")
	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	_, err := m.CreateLens(context.Background(), control.Lens{Name: "solo", WriteUID: alpha.UID()})
	require.NoError(t, err)

	repoCtx := repoCtxFor(alpha, laptop)
	lensCtx := lensCtxFor(t, m, laptop, "solo")
	lb, _ := repos.BindingFromContextOpt(lensCtx)
	rb, _ := repos.BindingFromContextOpt(repoCtx)
	require.False(t, lb.IsLens(), "a one-member lens passes the IsLens guard")
	require.True(t, lb.FromLens())
	require.True(t, strings.HasPrefix(lb.PinID(), "lens:"), lb.PinID())
	require.True(t, strings.HasPrefix(rb.PinID(), "repo:"), rb.PinID())

	args := map[string]any{"search": map[string]any{"path": "kb/u/"}, "depth": 0, "limit": 5}
	call := func(ctx context.Context, a map[string]any) (string, bool) {
		res, text := matCallRaw(t, ctx, m, a)
		return text, res.IsError
	}
	repoText, isErr := call(repoCtx, args)
	require.False(t, isErr, repoText)
	lensText, isErr := call(lensCtx, args)
	require.False(t, isErr, "the one-member lens is NOT refused: %s", lensText)

	var rp, lp materialize.FactSetPage
	require.NoError(t, json.Unmarshal([]byte(repoText), &rp))
	require.NoError(t, json.Unmarshal([]byte(lensText), &lp))
	require.NotNil(t, rp.Cursor)
	require.NotNil(t, lp.Cursor)
	roots := func(p materialize.FactSetPage) (out []string) {
		for _, n := range p.Facts {
			if n.TreeDepth == 0 && n.Unit > 0 {
				out = append(out, n.Path)
			}
		}
		return out
	}
	require.ElementsMatch(t, []string{"kb/u/r1.md", "kb/u/r2.md", "kb/u/r3.md"}, roots(rp), "the repo FactSet has these three roots")
	require.Equal(t, roots(rp), roots(lp), "same roots, same order")
	require.Equal(t, 3, lp.Units)
	id := func(c string) string { return c[:strings.IndexByte(c, '.')] }
	require.NotEqual(t, id(*rp.Cursor), id(*lp.Cursor), "two FactSets, two ids")
	norm := func(s string) string {
		s = strings.ReplaceAll(s, id(*rp.Cursor), "ID")
		return strings.ReplaceAll(s, id(*lp.Cursor), "ID")
	}
	require.Equal(t, norm(repoText), norm(lensText), "byte-equal pages except the cursor id")

	// The lens cursor carries the lens pin: its own endpoint pages it, the
	// repo endpoint (same principal, same read set, same session store) does
	// not, and says exactly what it says of an unknown id.
	again, isErr := call(lensCtx, map[string]any{"cursor": *lp.Cursor, "unit": 1})
	require.False(t, isErr, again)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	none, noneErr := call(repoCtx, map[string]any{"cursor": ghost, "unit": 1})
	got, gotErr := call(repoCtx, map[string]any{"cursor": *lp.Cursor, "unit": 1})
	require.True(t, noneErr, none)
	require.True(t, gotErr, "the repo endpoint must not page a lens-pinned cursor: %s", got)
	require.Equal(t, none, got, "refused like an unknown id")
	// And the repo cursor is refused on the lens in turn.
	back, backErr := call(lensCtx, map[string]any{"cursor": *rp.Cursor, "unit": 1})
	require.True(t, backErr, back)
	require.Equal(t, none, back)
}
