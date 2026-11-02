package kb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// The lens FactSet routes (F4): GET /lenses/{lens}/search/factset, its
// /export and DELETE are the repo routes' handlers over the lens binding. They
// replace #703's contract test that the route did NOT exist.

const lensFactSetBase = "/lenses/eng/search/factset"

// lensFactSetEnv is lens "eng" (write w, reads r1 and r2) over real stores:
//
//	w:  kb/technology/u/a1.md -> kb://<r1>/kb/technology/u/b1.md (another lens repo)
//	r1: kb/technology/u/b1.md, kb/technology/u/b2.md -> b1
//	r2: kb/technology/u/c1.md
//
// so a FactSet over path kb/technology/u/ has 4 units, one per result, in
// three members, and a1's ref into r1 is the lens_repo leaf.
type lensFactSetEnv struct {
	m              *repos.Manager
	w, r1, r2      *repos.RepoInstance
	router         http.Handler
	h, other       http.Handler
	repoBase       string // w's own FactSet route
	r1ID12, r2ID12 string
}

func newLensFactSetEnv(t *testing.T) lensFactSetEnv {
	t.Helper()
	m := newMatLensManager(t, "w", "r1", "r2")
	createLens(t, m, "eng", "w", "r1", "r2")
	e := lensFactSetEnv{m: m, w: m.Get("w"), r1: m.Get("r1"), r2: m.Get("r2")}
	e.r1ID12, e.r2ID12 = fact.ID12(e.r1.ID()), fact.ID12(e.r2.ID())
	writeMatFact(t, e.r1, e.r1.AgentBranch(), "kb/technology/u/b1.md", "B1", "b1 body.")
	writeMatFact(t, e.r1, e.r1.AgentBranch(), "kb/technology/u/b2.md", "B2", "b2 body.", "kb/technology/u/b1.md")
	writeMatFact(t, e.r2, e.r2.AgentBranch(), "kb/technology/u/c1.md", "C1", "c1 body.")
	writeMatFact(t, e.w, e.w.AgentBranch(), "kb/technology/u/a1.md", "A1", "a1 body.", "kb://"+e.r1ID12+"/kb/technology/u/b1.md")
	e.router = newTestRouter(m)
	e.h, e.other = asPrincipal("u1", e.router), asPrincipal("u2", e.router)
	e.repoBase = "/repos/w/branches/" + urlBranch(e.w.AgentBranch()) + "/search/factset"
	// Indexing of a write is asynchronous: wait until the lens lists all 4.
	var code int
	var body string
	require.Eventually(t, func() bool {
		code, _, body = getMaterialize(t, e.h, "/lenses/eng/facts?path=kb/technology/u/")
		return code == http.StatusOK && strings.Count(body, `"path":"`) >= 4 && strings.Contains(body, "c1.md") &&
			strings.Contains(body, "a1.md") && strings.Contains(body, "b2.md")
	}, 10*time.Second, 50*time.Millisecond, "the lens never listed the facts: %d %s", code, body)
	return e
}

// lensPages pages a lens FactSet on route base from first to the end.
func lensPages(t *testing.T, h http.Handler, base, first string) []materialize.FactSetPage {
	t.Helper()
	var out []materialize.FactSetPage
	u := first
	for {
		code, p, body := getFactSet(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		out = append(out, p)
		if !p.HasMore {
			return out
		}
		u = base + "?cursor=" + url.QueryEscape(*p.Cursor)
		require.Less(t, len(out), 100)
	}
}

// B2 replaced. The lens FactSet routes mirror the repo routes on a 3-member
// lens: page 1, cursor pages, a seek page, the export and the close, with
// the lens-only fields the engine adds (env.anchors of all three members,
// repo_name on every unit root, kb://<member>/ paths for read members, and the
// lens_repo leaf for a ref into another lens repo), the "All in lens <lens>"
// title, and the lens list's repo= narrowing (repeatable). repo= is a lens
// parameter only: the repo route answers it as any unknown parameter.
//
// Sabotage: register only the repo routes in Mount (the lens route is the
// unknown-route 404); or resolve the lens route's binding as its write repo
// (repos.NewBindingOfRepo(b.Write(), …): 1 unit, no anchors).
func TestLensFactSet_RESTRoutesMirrorTheRepoRoutes(t *testing.T) {
	e := newLensFactSetEnv(t)
	pages := lensPages(t, e.h, lensFactSetBase, lensFactSetBase+"?path=kb/technology/u/&depth=1&limit=1")
	p1 := pages[0]
	require.Equal(t, 4, p1.Units, "one unit per result, across the three members")
	require.Len(t, p1.Anchors, 3, "every searched member's anchor")
	for _, id := range []string{fact.ID12(e.w.ID()), e.r1ID12, e.r2ID12} {
		require.NotEmpty(t, p1.Anchors[id], id)
	}
	roots := map[string]string{}
	var leaves []materialize.FactSetNode
	for _, p := range pages {
		for _, n := range p.Facts {
			if n.Number == strconv.Itoa(n.Unit) {
				roots[n.Path] = n.RepoName
			}
		}
		leaves = append(leaves, p.Facts...)
	}
	require.Equal(t, map[string]string{
		"kb/technology/u/a1.md":                         "w",
		"kb://" + e.r1ID12 + "/kb/technology/u/b1.md": "r1",
		"kb://" + e.r1ID12 + "/kb/technology/u/b2.md": "r1",
		"kb://" + e.r2ID12 + "/kb/technology/u/c1.md": "r2",
	}, roots, "every unit root, with its repo")
	_, _, raw := getMaterialize(t, e.h, lensFactSetBase+"?path=kb/technology/u/&depth=1&limit=4")
	require.Contains(t, raw, `"reason":"lens_repo"`, "a1's ref into r1 is not followed")
	require.Contains(t, raw, `"repo_name":"r1"`)

	// Seek, export.
	code, seek, body := getFactSet(t, e.h, lensFactSetBase+"?cursor="+url.QueryEscape(*p1.Cursor)+"&unit=3")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 3, seek.UnitFrom)
	code, ctype, export := getMaterialize(t, e.h, lensFactSetBase+"/export?cursor="+url.QueryEscape(*p1.Cursor))
	require.Equal(t, http.StatusOK, code, export)
	require.Equal(t, "text/markdown; charset=utf-8", ctype)
	require.Equal(t, 4, strings.Count(export, "\n## "), "the export holds every unit: %s", export)

	// The no-filter title.
	code, all, body := getFactSet(t, e.h, lensFactSetBase+"?depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, materialize.FactSetLensTitle(materialize.FactSetSearch{Sort: materialize.SortRelevance}, "kb", "eng"), all.Title)
	require.Contains(t, all.Title, "eng")

	// repo= narrows to members by name, repeatable.
	code, nb, body := getFactSet(t, e.h, lensFactSetBase+"?path=kb/technology/u/&depth=0&limit=5&repo=r1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 2, nb.Units, "only r1's results")
	require.Equal(t, []string{"r1"}, nb.Search.Repos)
	for _, n := range nb.Facts {
		require.True(t, strings.HasPrefix(n.Path, "kb://"+e.r1ID12+"/"), n.Path)
	}
	code, nb, body = getFactSet(t, e.h, lensFactSetBase+"?path=kb/technology/u/&depth=0&limit=5&repo=r1&repo=r2")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 3, nb.Units, "r1's and r2's results")
	code, _, body = getMaterialize(t, e.h, e.repoBase+"?path=kb/technology/u/&repo=w")
	require.Equal(t, http.StatusBadRequest, code, body)
	require.Contains(t, body, `unknown parameter \"repo\"`)

	// Close: 204, then the cursor reads as none, and a second close is 404.
	code, body = deleteFactSet(t, e.h, lensFactSetBase+"?cursor="+url.QueryEscape(*p1.Cursor))
	require.Equal(t, http.StatusNoContent, code, body)
	require.Empty(t, body)
	code, _, _ = getFactSet(t, e.h, lensFactSetBase+"?cursor="+url.QueryEscape(*p1.Cursor))
	require.Equal(t, http.StatusNotFound, code)
	code, _ = deleteFactSet(t, e.h, lensFactSetBase+"?cursor="+url.QueryEscape(*p1.Cursor))
	require.Equal(t, http.StatusNotFound, code)
}

// problem is the exact body hal.WriteProblem writes for these values.
func problem(t *testing.T, status int, title, detail, instance string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	hal.WriteProblem(rec, status, title, detail, instance)
	return rec.Body.String()
}

// serve sends method u through h.
func serve(h http.Handler, method, u string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, u, nil))
	return rec
}

// engineLensErr is the engine's own error for a first call of req on lens
// "eng": what the REST route must pass through, never restate.
func engineLensErr(t *testing.T, e lensFactSetEnv, req materialize.FactSetRequest) error {
	t.Helper()
	ctx, err := repos.ResolveLensBinding(auth.WithRepoReach(auth.WithPrincipal(context.Background(),
		auth.Principal{Kind: auth.KindHost, ID: "u1", Via: auth.ViaSocket}), auth.AllRepos), e.m, "eng")
	require.NoError(t, err)
	b := repos.BindingFromContext(ctx)
	req.Binding, req.Manager = b, e.m
	var ferr error
	require.NoError(t, b.Write().WithRead(func(svc *store.Service) {
		sc := materialize.CursorScope{Sessions: svc.ToolSession(), Branch: b.WriteMountBranch(), Pin: b.PinID(),
			ReadSet: federate.ReadSetFingerprint(b), Owner: materialize.FactSetOwner(ctx)}
		_, _, ferr = materialize.FactSetFirstPage(ctx, sc, req)
	}))
	require.Error(t, ferr)
	return ferr
}

// Error mapping on the lens routes, each answer equal to the route it mirrors:
//   - an unknown cursor, another principal's cursor, a closed one, and one
//     whose lens lost a member since it was made: the repo route's 404, byte
//     for byte (the instance path aside);
//   - cross_kb=true: 400 "Invalid request" with the ENGINE's text, passed
//     through;
//   - repo= naming no member: 422 "Unknown repo", the lens list's own answer
//     (GET /lenses/{lens}/search?repo=…), byte for byte;
//   - busy (the FactSet memory budget is full past its wait): 503 with the
//     repo route's Retry-After and body, on a first call and on the export;
//   - malformed parameters: 400.
//
// Sabotage: drop the *federate.UnknownRepoError arm of writeFactSetError
// (the unknown repo is the generic 400), or let the engine wrap it with %v
// again (errors.As no longer finds it: 400).
func TestLensFactSet_ErrorMappingMatchesTheRepoAndLensListRoutes(t *testing.T) {
	e := newLensFactSetEnv(t)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	asRepo := func(body string) string { return strings.ReplaceAll(body, lensFactSetBase, e.repoBase) }

	// 404s.
	rc, _, repoGhost := getMaterialize(t, e.h, e.repoBase+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, rc)
	lc, _, lensGhost := getMaterialize(t, e.h, lensFactSetBase+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, lc, lensGhost)
	require.Equal(t, repoGhost, asRepo(lensGhost), "the repo route's 404")
	code, p1, body := getFactSet(t, e.h, lensFactSetBase+"?path=kb/technology/u/&depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur := url.QueryEscape(*p1.Cursor)
	code, _, foreign := getMaterialize(t, e.other, lensFactSetBase+"?cursor="+cur)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, lensGhost, foreign, "another principal's cursor reads like none")
	code, _, foreign = getMaterialize(t, e.other, lensFactSetBase+"/export?cursor="+cur)
	require.Equal(t, http.StatusNotFound, code, foreign)
	code, foreignClose := deleteFactSet(t, e.other, lensFactSetBase+"?cursor="+cur)
	require.Equal(t, http.StatusNotFound, code)
	_, ghostClose := deleteFactSet(t, e.other, lensFactSetBase+"?cursor="+ghost)
	require.Equal(t, ghostClose, foreignClose)
	code, _, body = getMaterialize(t, e.h, lensFactSetBase+"?cursor="+cur)
	require.Equal(t, http.StatusOK, code, "nothing was consumed or closed: %s", body)

	// cross_kb: the engine's refusal, passed through.
	engErr := engineLensErr(t, e, materialize.FactSetRequest{
		Search:  materialize.FactSetSearch{Path: "kb/technology/u/", Sort: materialize.SortRelevance},
		Options: materialize.Options{Depth: 1, MaxNodes: 10, CrossKB: true}, Facts: 10, Limit: 1,
	})
	code, _, body = getMaterialize(t, e.h, lensFactSetBase+"?path=kb/technology/u/&cross_kb=true")
	require.Equal(t, http.StatusBadRequest, code, body)
	require.Equal(t, problem(t, http.StatusBadRequest, "Invalid request", engErr.Error(), lensFactSetBase), body)

	// Unknown repo: the lens list's 422.
	code, _, listBody := getMaterialize(t, e.h, "/lenses/eng/search?path=kb/technology/u/&repo=nope")
	require.Equal(t, http.StatusUnprocessableEntity, code, listBody)
	code, _, body = getMaterialize(t, e.h, lensFactSetBase+"?path=kb/technology/u/&repo=nope")
	require.Equal(t, http.StatusUnprocessableEntity, code, body)
	require.Equal(t, listBody, strings.ReplaceAll(body, lensFactSetBase, "/lenses/eng/search"), "the lens list's own answer")
	code, _, body = getMaterialize(t, e.h, lensFactSetBase+"?repo=w&repo=nope")
	require.Equal(t, http.StatusUnprocessableEntity, code, body)

	// Malformed parameters.
	for _, bad := range []string{
		"?nosuch=1", "?cursor=" + cur + "&repo=r1", "?cursor=" + cur + "&unit=0", "?cursor=",
		"?path=kb/technology/u/&facts=x", "?path=kb/technology/u/&sort=sideways",
	} {
		code, _, body := getMaterialize(t, e.h, lensFactSetBase+bad)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", bad, body)
	}
	code, _, body = getMaterialize(t, e.h, lensFactSetBase+"/export?cursor="+cur+"&unit=1")
	require.Equal(t, http.StatusBadRequest, code, body)
	code, body = deleteFactSet(t, e.h, lensFactSetBase)
	require.Equal(t, http.StatusBadRequest, code, body)

	// Busy: the repo route's 503, on a first call and on the export.
	code, rp, body := getFactSet(t, e.h, e.repoBase+"?path=kb/technology/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	func() {
		defer materialize.SetFactSetCacheBudget(0)
		materialize.SetFactSetCacheBudget(1)
		defer materialize.SetFactSetBudgetWaitForTest(50 * time.Millisecond)()
		release := materialize.HoldFactSetPlanningMemoryForTest(1)
		defer release()
		busy := func(u string) (string, string) {
			rec := serve(e.h, http.MethodGet, u)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s: %s", u, rec.Body.String())
			return rec.Header().Get("Retry-After"), rec.Body.String()
		}
		repoRetry, repoBusy := busy(e.repoBase + "?path=kb/technology/u/")
		lensRetry, lensBusy := busy(lensFactSetBase + "?path=kb/technology/u/")
		require.Equal(t, materializeBusyRetryAfter, lensRetry)
		require.Equal(t, repoRetry, lensRetry)
		require.Equal(t, repoBusy, asRepo(lensBusy), "the repo route's 503")
		_, repoBusy = busy(e.repoBase + "/export?cursor=" + url.QueryEscape(*rp.Cursor))
		lensRetry, lensBusy = busy(lensFactSetBase + "/export?cursor=" + cur)
		require.Equal(t, materializeBusyRetryAfter, lensRetry)
		require.Equal(t, repoBusy, asRepo(lensBusy))
	}()

	// The lens loses a member: the cursor reads like none.
	_, err := e.m.UpdateLens(context.Background(), control.Lens{Name: "eng", WriteUID: e.w.UID(),
		Reads: []control.LensRead{{RepoUID: e.r1.UID()}}})
	require.NoError(t, err)
	code, _, gone := getMaterialize(t, e.h, lensFactSetBase+"?cursor="+cur)
	require.Equal(t, http.StatusNotFound, code, gone)
	require.Equal(t, lensGhost, gone, "a membership change reads like an unknown cursor")
}

// Cursors do not cross between a lens and its write repo (the pins differ:
// lens:<uid> against repo:<uid>): the lens cursor on w's own routes, and w's
// cursor on the lens routes, are each answered exactly as an unknown cursor
// on that route (page, export, close), and nothing is closed.
//
// The pin and the read set each refuse it on their own (redundant by design),
// so this is the REST twin of TestFactSet_Lens_CursorsDoNotCrossEndpoints.
func TestLensFactSet_CursorsDoNotCrossRepoAndLensRoutes(t *testing.T) {
	e := newLensFactSetEnv(t)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	code, lp, body := getFactSet(t, e.h, lensFactSetBase+"?path=kb/technology/u/&depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, rp, body := getFactSet(t, e.h, e.repoBase+"?path=kb/technology/u/&depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 4, lp.Units)
	require.Equal(t, 1, rp.Units)
	for _, c := range []struct{ base, foreign string }{
		{e.repoBase, *lp.Cursor},
		{lensFactSetBase, *rp.Cursor},
	} {
		for _, suffix := range []string{"?cursor=", "/export?cursor="} {
			_, _, none := getMaterialize(t, e.h, c.base+suffix+ghost)
			code, _, got := getMaterialize(t, e.h, c.base+suffix+url.QueryEscape(c.foreign))
			require.Equal(t, http.StatusNotFound, code, "%s%s: %s", c.base, suffix, got)
			require.Equal(t, none, got)
		}
		_, none := deleteFactSet(t, e.h, c.base+"?cursor="+ghost)
		code, got := deleteFactSet(t, e.h, c.base+"?cursor="+url.QueryEscape(c.foreign))
		require.Equal(t, http.StatusNotFound, code, got)
		require.Equal(t, none, got)
	}
	for base, c := range map[string]string{lensFactSetBase: *lp.Cursor, e.repoBase: *rp.Cursor} {
		code, _, body := getMaterialize(t, e.h, base+"?cursor="+url.QueryEscape(c)+"&unit=1")
		require.Equal(t, http.StatusOK, code, "each still pages on its own route: %s", body)
	}
}

// A one-member lens (ruling Lens 6 / Q-L9) is served on the repository path:
// the lens route's page is the write repo's own FactSet page, byte for byte
// but the cursor id; repo= accepts exactly the write repo's name (narrowing
// nothing), and any other name is the lens list's 422 "Unknown repo".
//
// Sabotage: wrap checkRepoNarrowing's UnknownRepoError with %v (the 422 is
// the generic 400).
func TestLensFactSet_OneMemberLensIsTheWriteRepoFactSet(t *testing.T) {
	m := newMatLensManager(t, "w")
	createLens(t, m, "solo", "w")
	w := m.Get("w")
	writeMatFact(t, w, w.AgentBranch(), "kb/technology/u/a1.md", "A1", "a1 body.")
	writeMatFact(t, w, w.AgentBranch(), "kb/technology/u/a2.md", "A2", "a2 body.", "kb/technology/u/a1.md")
	h := asPrincipal("u1", newTestRouter(m))
	repoBase := "/repos/w/branches/" + urlBranch(w.AgentBranch()) + "/search/factset"
	const soloBase = "/lenses/solo/search/factset"
	var code int
	var body string
	require.Eventually(t, func() bool {
		code, _, body = getMaterialize(t, h, "/lenses/solo/facts?path=kb/technology/u/")
		return code == http.StatusOK && strings.Contains(body, "a1.md") && strings.Contains(body, "a2.md")
	}, 10*time.Second, 50*time.Millisecond, "the lens never listed the facts: %d %s", code, body)

	norm := func(raw string) string {
		var p struct {
			Cursor *string `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &p), raw)
		require.NotNil(t, p.Cursor, raw)
		return strings.ReplaceAll(raw, mustCursorID(t, *p.Cursor), "ID")
	}
	code, _, repoPage := getMaterialize(t, h, repoBase+"?path=kb/technology/u/&depth=1&limit=2")
	require.Equal(t, http.StatusOK, code, repoPage)
	code, _, lensPage := getMaterialize(t, h, soloBase+"?path=kb/technology/u/&depth=1&limit=2")
	require.Equal(t, http.StatusOK, code, lensPage)
	require.Equal(t, norm(repoPage), norm(lensPage), "the write repo's FactSet page")
	// repo=w narrows nothing: the same units and sections (the search echoes it).
	code, rp, body := getFactSet(t, h, repoBase+"?path=kb/technology/u/&depth=1&limit=2")
	require.Equal(t, http.StatusOK, code, body)
	code, np, body := getFactSet(t, h, soloBase+"?path=kb/technology/u/&depth=1&limit=2&repo=w")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []string{"w"}, np.Search.Repos)
	require.Equal(t, rp.Units, np.Units)
	require.Equal(t, rp.Facts, np.Facts)
	code, _, listBody := getMaterialize(t, h, "/lenses/solo/search?repo=nope")
	require.Equal(t, http.StatusUnprocessableEntity, code, listBody)
	code, _, body = getMaterialize(t, h, soloBase+"?repo=nope")
	require.Equal(t, http.StatusUnprocessableEntity, code, body)
	require.Equal(t, listBody, strings.ReplaceAll(body, soloBase, "/lenses/solo/search"))
}
