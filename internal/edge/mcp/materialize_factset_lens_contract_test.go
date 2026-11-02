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
// facade test below, and since the user's 2026-11-02 ruling a one-member lens
// is the lens facade over a single sub, with a multi-member lens's shape.

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

// B3 (MCP half), as the user ruled on 2026-11-02 ("lens HAS TO BE
// consistent, no matter how many repos are in it"). A lens whose only member
// is the write repo is not IsLens (it federates across nothing) but IS a lens
// FactSet (materialize.LensFactSet), served by the lens facade over a single
// sub:
//   - its page is the multi-member lens's page for the same search narrowed
//     to the same member (search.repo = ["alpha"] on "eng" = alpha + beta),
//     byte for byte except the cursor id;
//   - against the write repo's own page it has the same roots, in the same
//     order, and differs ONLY by the lens fields: env.anchors (the one
//     member's anchor) and repo_name on every unit root, nowhere else;
//   - its cursor is pinned lens:<uid>, so the repo endpoint (pin repo:<uid>)
//     cannot page it: it reads exactly like an unknown id.
//
// Sabotage: dispatch FactSetFirstPage on IsLens alone (the solo page has no
// anchors and no repo_name: the "eng" comparison fails); or give the lens
// the repo pin (the repo call pages the lens cursor).
func TestFactSet_Contract_OneMemberLensIsTheLensFacadeWithTheLensPin(t *testing.T) {
	m, alpha, _, _, _, lens := newLensE2E(t)
	obs(t, alpha, "kb/architecture/u/r1.md", "R1")
	obs(t, alpha, "kb/architecture/u/r2.md", "R2", "kb/architecture/u/r1.md")
	obs(t, alpha, "kb/architecture/u/r3.md", "R3")
	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	_, err := m.CreateLens(context.Background(), control.Lens{Name: "solo", WriteUID: alpha.UID()})
	require.NoError(t, err)

	repoCtx := repoCtxFor(alpha, laptop)
	lensCtx := lensCtxFor(t, m, laptop, "solo")
	lb, _ := repos.BindingFromContextOpt(lensCtx)
	rb, _ := repos.BindingFromContextOpt(repoCtx)
	require.False(t, lb.IsLens(), "a one-member lens is not IsLens (breadth)")
	require.True(t, lb.FromLens())
	require.True(t, materialize.LensFactSet(lb), "but it is a lens FactSet")
	require.False(t, materialize.LensFactSet(rb))
	require.True(t, strings.HasPrefix(lb.PinID(), "lens:"), lb.PinID())
	require.True(t, strings.HasPrefix(rb.PinID(), "repo:"), rb.PinID())

	args := map[string]any{"search": map[string]any{"path": "kb/architecture/u/"}, "depth": 1, "limit": 5}
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
	require.NotNil(t, lp.Cursor, lensText)
	roots := func(p materialize.FactSetPage) (out []string) {
		for _, n := range p.Facts {
			if n.TreeDepth == 0 && n.Unit > 0 {
				out = append(out, n.Path)
			}
		}
		return out
	}
	require.ElementsMatch(t, []string{"kb/architecture/u/r1.md", "kb/architecture/u/r2.md", "kb/architecture/u/r3.md"}, roots(rp), "the repo FactSet has these three roots")
	require.Equal(t, roots(rp), roots(lp), "same roots, same order")
	require.Equal(t, 3, lp.Units)
	id := func(c string) string { return c[:strings.IndexByte(c, '.')] }
	require.NotEqual(t, id(*rp.Cursor), id(*lp.Cursor), "two FactSets, two ids")

	// The multi-member lens narrowed to alpha: the same page, byte for byte.
	narrowed := map[string]any{"search": map[string]any{"path": "kb/architecture/u/", "repo": []any{"alpha"}}, "depth": 1, "limit": 5}
	multiText, isErr := call(lensCtxFor(t, m, laptop, lens), narrowed)
	require.False(t, isErr, multiText)
	soloText, isErr := call(lensCtx, narrowed)
	require.False(t, isErr, soloText)
	require.Equal(t, withoutCursorID(t, multiText), withoutCursorID(t, soloText), "the multi-member lens's page shape")

	// Against the repo page: the lens fields and nothing else.
	alpha12 := fact.ID12(alpha.ID())
	require.Equal(t, map[string]string{alpha12: lp.Anchor}, lp.Anchors, "env.anchors: the one member's anchor")
	require.NotEmpty(t, lp.Anchor)
	require.Nil(t, rp.Anchors, "control: a repository page has no anchors")
	unitRoots := 0
	for _, n := range lp.Facts {
		if n.Number == strconv.Itoa(n.Unit) {
			unitRoots++
			require.Equal(t, "alpha", n.RepoName, "repo_name on unit root %s", n.Number)
		} else {
			require.Empty(t, n.RepoName, "repo_name on unit roots only: %s", n.Number)
		}
	}
	require.Equal(t, 3, unitRoots)
	for _, n := range rp.Facts {
		require.Empty(t, n.RepoName, "control: a repository page has no repo_name")
	}
	require.Equal(t, withoutLensFields(t, withoutCursorID(t, repoText)), withoutLensFields(t, withoutCursorID(t, lensText)),
		"the repo page plus env.anchors and repo_name, nothing else")

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

// A one-member lens is a lens (user ruling 2026-11-02, M1), on MCP too:
// cross_kb=true on the "solo" lens endpoint is the multi-member lens's tool
// error, text for text, and writes no FactSet row; with no filter its title
// is "All in lens solo". Controls: the write repo's own endpoint accepts
// cross_kb and keeps the repo title.
//
// Sabotage: dispatch FactSetFirstPage on IsLens alone (the solo call is
// served on the repo path: cross_kb accepted, alpha's title).
func TestFactSet_Lens_OneMemberLensIsALens(t *testing.T) {
	m, alpha, _, _, _, lens := newLensE2E(t)
	obs(t, alpha, "kb/u/r1.md", "R1")
	laptop := auth.Principal{Kind: auth.KindHost, ID: "laptop", Via: auth.ViaToken}
	_, err := m.CreateLens(context.Background(), control.Lens{Name: "solo", WriteUID: alpha.UID()})
	require.NoError(t, err)
	soloCtx, multiCtx, repoCtx := lensCtxFor(t, m, laptop, "solo"), lensCtxFor(t, m, laptop, lens), repoCtxFor(alpha, laptop)

	cross := map[string]any{"search": map[string]any{"path": "kb/u/"}, "cross_kb": true}
	res, multi := matCallRaw(t, multiCtx, m, cross)
	require.True(t, res.IsError, multi)
	before := factsetWrites(t, alpha)
	res, solo := matCallRaw(t, soloCtx, m, cross)
	require.True(t, res.IsError, "cross_kb on a one-member lens is refused: %s", solo)
	require.Equal(t, multi, solo, "the multi-member lens's refusal")
	require.Equal(t, before, factsetWrites(t, alpha), "nothing written")
	res, text := matCallRaw(t, repoCtx, m, cross)
	require.False(t, res.IsError, "control: the repository accepts cross_kb: %s", text)

	title := func(ctx context.Context) string {
		t.Helper()
		res, text := matCallRaw(t, ctx, m, map[string]any{"search": map[string]any{}, "depth": 0, "limit": 1})
		require.False(t, res.IsError, text)
		var p materialize.FactSetPage
		require.NoError(t, json.Unmarshal([]byte(text), &p))
		return p.Title
	}
	none := materialize.FactSetSearch{Sort: materialize.SortRelevance}
	require.Equal(t, materialize.FactSetLensTitle(none, alpha.OntologyRoot(), "solo"), title(soloCtx))
	require.Equal(t, materialize.FactSetTitle(none, alpha.OntologyRoot(), alpha.Name()), title(repoCtx), "control: the repository's title")
	require.NotEqual(t, title(repoCtx), title(soloCtx))
}

// withoutCursorID is a FactSet page with its cursor's FactSet id replaced by
// "ID" (each first call mints its own id).
func withoutCursorID(t *testing.T, page string) string {
	t.Helper()
	var p materialize.FactSetPage
	require.NoError(t, json.Unmarshal([]byte(page), &p), page)
	if p.Cursor == nil {
		return page
	}
	return strings.ReplaceAll(page, (*p.Cursor)[:strings.IndexByte(*p.Cursor, '.')], "ID")
}

// withoutLensFields is a FactSet page with the lens-only wire fields removed:
// env.anchors and every node's repo_name.
func withoutLensFields(t *testing.T, page string) string {
	t.Helper()
	var p map[string]any
	require.NoError(t, json.Unmarshal([]byte(page), &p), page)
	delete(p, "anchors")
	facts, _ := p["facts"].([]any)
	for _, f := range facts {
		delete(f.(map[string]any), "repo_name")
	}
	out, err := json.Marshal(p)
	require.NoError(t, err)
	return string(out)
}
