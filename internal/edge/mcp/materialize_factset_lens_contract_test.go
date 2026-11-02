package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// Lens FactSet CONTRACT tests. PR A pinned what a FactSet did through a lens
// endpoint; the lens facade (F3) replaced its multi-member refusal with the
// facade test below, and the one-member path stays a contract (ruling Lens 6:
// a one-member lens stays on the repo path).

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

// B1 (MCP half), as the lens facade (F3) rules it; it replaced PR A's
// refusal test. knomit_materialize factset mode on a MULTI-member lens
// endpoint materializes the lens's list:
//   - page 1 serves both members' roots (beta's as kb://<beta>/…), each
//     unit root naming its repo, with an anchors map of both members;
//   - the page is byte for byte the engine's FactSetFirstPage for the same
//     request, scope and binding (one builder; REST joins in F4);
//   - the cursor pages, seeks (`unit`) and closes (`close: true`); a closed
//     cursor answers the expired text;
//   - ONE session row per FactSet, in alpha's (the write repo's) store, none
//     in beta's;
//   - cross_kb=true is a tool error (ruling Lens 2); search.repo narrows to
//     members by name and an unknown name is the lens list's own error
//     (ruling Lens 6).
//
// Sabotage: put the IsLens refusal back in FactSetFirstPage; every lens call
// is a tool error and the first assertion fails.
func TestFactSet_Lens_MCPServesTheLensFactSet(t *testing.T) {
	m, alpha, beta, _, _, lens := newLensE2E(t)
	obs(t, alpha, "kb/architecture/u/a1.md", "A1")
	obs(t, beta, "kb/architecture/u/b1.md", "B1")
	obs(t, beta, "kb/architecture/u/b2.md", "B2", "kb/architecture/u/b1.md")
	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	ctx := lensCtxFor(t, m, laptop, lens)
	lb, ok := repos.BindingFromContextOpt(ctx)
	require.True(t, ok)
	require.True(t, lb.IsLens(), "the fixture lens is a multi-member lens (alpha + beta)")
	betaID := fact.ID12(beta.ID())

	wa, wb := factsetWrites(t, alpha), factsetWrites(t, beta)
	args := map[string]any{"search": map[string]any{"path": "kb/architecture/u/"}, "depth": 1, "limit": 1}
	res, text := matCallRaw(t, ctx, m, args)
	require.False(t, res.IsError, "a factset on a lens endpoint is served: %s", text)
	var p materialize.FactSetPage
	require.NoError(t, json.Unmarshal([]byte(text), &p))
	require.Equal(t, 3, p.Units)
	require.Len(t, p.Anchors, 2)
	require.Contains(t, p.Anchors, betaID)
	require.Equal(t, wa+1, factsetWrites(t, alpha), "ONE row, in the write repo's store")
	require.Equal(t, wb, factsetWrites(t, beta), "no row in the read member's store")

	// Byte identity with the engine call over the same binding and scope.
	sWrite, release, err := storeIndices(lb.Write())
	require.NoError(t, err)
	defer release()
	ep, _, err := materialize.FactSetFirstPage(ctx, factsetScope(ctx, lb, sWrite), materialize.FactSetRequest{
		Binding: lb, Manager: m, MayRead: callerMayRead(ctx), Search: materialize.FactSetSearch{Path: "kb/architecture/u/", Sort: materialize.SortRelevance},
		Options: materialize.Options{Depth: 1, MaxNodes: materialize.DefaultOptions().MaxNodes}, Facts: materialize.DefaultFactSetFacts, Limit: 1,
		Clusters: mcpMotifClusters,
	})
	require.NoError(t, err)
	id := func(c *string) string { return (*c)[:strings.IndexByte(*c, '.')] }
	norm := func(s, c string) string { return strings.ReplaceAll(s, c, "ID") }
	eraw, err := json.Marshal(ep)
	require.NoError(t, err)
	require.Equal(t, norm(string(eraw), id(ep.Cursor)), norm(text, id(p.Cursor)), "the MCP page is the engine's page")

	// Page through, seek, close.
	var roots []string
	names := map[string]string{}
	cur := p
	for {
		for _, n := range cur.Facts {
			if n.Number == strconv.Itoa(n.Unit) {
				roots = append(roots, n.Path)
				names[n.Path] = n.RepoName
			}
		}
		if !cur.HasMore {
			break
		}
		res, text = matCallRaw(t, ctx, m, map[string]any{"cursor": *cur.Cursor})
		require.False(t, res.IsError, text)
		cur = materialize.FactSetPage{}
		require.NoError(t, json.Unmarshal([]byte(text), &cur))
	}
	require.ElementsMatch(t, []string{"kb/architecture/u/a1.md", "kb://" + betaID + "/kb/architecture/u/b1.md", "kb://" + betaID + "/kb/architecture/u/b2.md"}, roots)
	require.Equal(t, "alpha", names["kb/architecture/u/a1.md"])
	require.Equal(t, "beta", names["kb://"+betaID+"/kb/architecture/u/b1.md"])
	res, text = matCallRaw(t, ctx, m, map[string]any{"cursor": *p.Cursor, "unit": 3})
	require.False(t, res.IsError, text)
	var seek materialize.FactSetPage
	require.NoError(t, json.Unmarshal([]byte(text), &seek))
	require.Equal(t, 3, seek.UnitFrom)
	res, text = matCallRaw(t, ctx, m, map[string]any{"close": true, "cursor": *p.Cursor})
	require.False(t, res.IsError, text)
	require.Equal(t, `{"closed":true}`, text)
	res, text = matCallRaw(t, ctx, m, map[string]any{"cursor": *p.Cursor})
	require.True(t, res.IsError)
	require.Equal(t, errMaterializeExpired, text)

	// Ruling Lens 2: cross_kb on a lens is refused, nothing written.
	before := factsetWrites(t, alpha)
	res, text = matCallRaw(t, ctx, m, map[string]any{"search": map[string]any{"path": "kb/architecture/u/"}, "cross_kb": true})
	require.True(t, res.IsError, text)
	require.Equal(t, "invalid materialize request: cross_kb is not accepted on a lens factset", text)
	require.Equal(t, before, factsetWrites(t, alpha))

	// Ruling Lens 6: repo narrowing, the lens list's filter.
	res, text = matCallRaw(t, ctx, m, map[string]any{"search": map[string]any{"path": "kb/architecture/u/", "repo": []any{"beta"}}, "depth": 0, "limit": 5})
	require.False(t, res.IsError, text)
	var nb materialize.FactSetPage
	require.NoError(t, json.Unmarshal([]byte(text), &nb))
	require.Equal(t, 2, nb.Units, "only beta's results")
	require.Equal(t, []string{"beta"}, nb.Search.Repos)
	for _, n := range nb.Facts {
		require.True(t, strings.HasPrefix(n.Path, "kb://"+betaID+"/"), n.Path)
	}
	res, text = matCallRaw(t, ctx, m, map[string]any{"search": map[string]any{"path": "kb/architecture/u/", "repo": []any{"nope"}}})
	require.True(t, res.IsError, text)
	require.Equal(t, `invalid materialize request: no mount named "nope" in lens "eng"`, text)
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
